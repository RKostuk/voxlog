package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/output"
	"voxlog-go/internal/settings"
)

func TestMixAudioSumsAndClips(t *testing.T) {
	got := mixAudio([]float32{0.5, -0.5, 0.9}, []float32{0.25, -0.25, 0.9})
	want := []float32{0.75, -0.75, 1.0} // third sample clipped from 1.8
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestMixAudioClipsNegativeOverflow(t *testing.T) {
	got := mixAudio([]float32{-0.9}, []float32{-0.9})
	if got[0] != -1.0 {
		t.Fatalf("got %v, want -1 (clipped)", got[0])
	}
}

func TestMixAudioUnevenLengthsKeepsLongerAndShorterContributesZero(t *testing.T) {
	got := mixAudio([]float32{0.1, 0.2, 0.3, 0.4}, []float32{0.1})
	want := []float32{0.2, 0.2, 0.3, 0.4}
	if len(got) != 4 {
		t.Fatalf("got length %d, want 4", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMixAudioEmptyStreamReturnsTheOther(t *testing.T) {
	mic := []float32{0.1, 0.2}
	if got := mixAudio(mic, nil); len(got) != 2 || got[0] != 0.1 {
		t.Fatalf("empty system audio should pass the mic through, got %v", got)
	}
	if got := mixAudio(nil, mic); len(got) != 2 || got[0] != 0.1 {
		t.Fatalf("empty mic should pass the system audio through, got %v", got)
	}
	if got := mixAudio(nil, nil); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/Documents/Voxlog"); got != filepath.Join(home, "Documents", "Voxlog") {
		t.Fatalf("got %q", got)
	}
	// Only a leading "~/" is a home reference; everything else is literal.
	for _, in := range []string{"/tmp/voxlog", "relative/path", "~weird", ""} {
		if got := expandHome(in); got != in {
			t.Fatalf("expandHome(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestModelForSettingsMatchesFamilyAndVariant(t *testing.T) {
	cfg := settings.DefaultSettings()
	spec, ok := modelForSettings(cfg)
	if !ok {
		t.Fatalf("default settings %s/%s should resolve to a known model", cfg.ModelFamily, cfg.ModelVariant)
	}
	if spec.Family != cfg.ModelFamily || spec.Variant != cfg.ModelVariant {
		t.Fatalf("got %s/%s", spec.Family, spec.Variant)
	}
	if len(spec.Files) == 0 {
		t.Fatal("resolved spec carries no files, so nothing could ever be downloaded")
	}
}

func TestModelForSettingsUnknownVariantFails(t *testing.T) {
	cfg := settings.DefaultSettings()
	cfg.ModelVariant = "not-a-real-variant"
	if _, ok := modelForSettings(cfg); ok {
		t.Fatal("a family match alone must not resolve: the variant selects the files")
	}
}

// knownModels is what the Settings window offers and what modelForSettings
// resolves against; the ASR engines load "encoder.onnx"/"decoder.onnx"/
// "joiner.onnx"/"tokens.txt" by those exact hardcoded names, so a spec that
// downloads a file under any other name silently produces a model directory
// the engine cannot open.
func TestKnownModelsUseTheFilenamesTheEnginesLoad(t *testing.T) {
	allowed := map[string]bool{
		"encoder.onnx":           true,
		"decoder.onnx":           true,
		"joiner.onnx":            true,
		"tokens.txt":             true,
		"encoder.weights":        true, // ONNX external data, loaded by name from encoder.onnx
		"encoder.int8.onnx.data": true, // same, for canary's encoder
	}
	for _, m := range knownModels {
		if len(m.Files) == 0 {
			t.Errorf("%s/%s has no files", m.Family, m.Variant)
		}
		seen := map[string]bool{}
		for _, f := range m.Files {
			if !allowed[f.Filename] {
				t.Errorf("%s/%s: filename %q is not one the engines load", m.Family, m.Variant, f.Filename)
			}
			if seen[f.Filename] {
				t.Errorf("%s/%s: duplicate filename %q -- one URL would overwrite the other", m.Family, m.Variant, f.Filename)
			}
			seen[f.Filename] = true
			if f.URL == "" {
				t.Errorf("%s/%s: %q has no URL", m.Family, m.Variant, f.Filename)
			}
		}
		if !seen["tokens.txt"] {
			t.Errorf("%s/%s: no tokens.txt, which every sherpa-onnx recognizer requires", m.Family, m.Variant)
		}
	}
}

func TestKnownModelsStreamingOnlyWhereSupported(t *testing.T) {
	for _, m := range knownModels {
		// online.go is the only streaming engine and NewTranscriber routes to
		// it by family name -- any other family claiming streaming would hand
		// main.go a plain Transcriber and silently fall back to batch.
		if m.SupportsStreaming && m.Family != "nemotron" {
			t.Errorf("%s/%s claims streaming, but only the nemotron family has an online engine", m.Family, m.Variant)
		}
	}
}

// writeModelFiles lays out a downloaded-looking model directory for spec.
func writeModelFiles(t *testing.T, baseDir string, spec asr.ModelSpec) {
	t.Helper()
	dir := asr.ModelDir(baseDir, spec)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range spec.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Filename), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNeedsSetupWhenModelNotDownloaded(t *testing.T) {
	if !needsSetup(settings.DefaultSettings(), t.TempDir()) {
		t.Fatal("an empty models dir means dictation is impossible -- expected setup")
	}
}

func TestNeedsSetupFalseOnceModelPresent(t *testing.T) {
	base := t.TempDir()
	cfg := settings.DefaultSettings()
	spec, ok := modelForSettings(cfg)
	if !ok {
		t.Fatal("default model should be known")
	}
	writeModelFiles(t, base, spec)

	if needsSetup(cfg, base) {
		t.Fatal("model downloaded and a dictate key bound: nothing left to set up")
	}
}

func TestNeedsSetupWhenNoDictateKey(t *testing.T) {
	base := t.TempDir()
	cfg := settings.DefaultSettings()
	spec, _ := modelForSettings(cfg)
	writeModelFiles(t, base, spec)

	cfg.DictateKeyID = ""
	if !needsSetup(cfg, base) {
		t.Fatal("no hotkey means no way to start a dictation -- expected setup")
	}
}

func TestNeedsSetupWhenModelUnknown(t *testing.T) {
	cfg := settings.DefaultSettings()
	cfg.ModelFamily = "gone-from-knownModels"
	if !needsSetup(cfg, t.TempDir()) {
		t.Fatal("a model that no longer exists must be treated as unconfigured")
	}
}

// fakeTranscriber records how many times it was built and closed, so the
// cache's reuse/rebuild decisions are observable without loading a real model.
type fakeTranscriber struct{ closed int }

func (f *fakeTranscriber) Transcribe([]float32, string) (string, error) { return "", nil }
func (f *fakeTranscriber) Close()                                       { f.closed++ }

// transcriberCache.get calls asr.NewTranscriber directly, so the seam these
// tests use is the cache's own state: prime it with a fake, then assert what
// a subsequent get does with it.
func TestTranscriberCacheReusesSameModelAndLanguage(t *testing.T) {
	spec := asr.ModelSpec{Family: "whisper", Variant: "large-v3"}
	inner := &fakeTranscriber{}
	c := &transcriberCache{spec: spec, language: "uk", inner: inner}

	got, err := c.get(spec, "/unused", "uk")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != inner {
		t.Fatal("same model and language should return the cached transcriber, not rebuild")
	}
	if inner.closed != 0 {
		t.Fatal("cached transcriber must not be closed while still in use")
	}
}

func TestTranscriberCacheRebuildsOnLanguageChange(t *testing.T) {
	spec := asr.ModelSpec{Family: "whisper", Variant: "large-v3"}
	inner := &fakeTranscriber{}
	c := &transcriberCache{spec: spec, language: "uk", inner: inner}

	// Language is part of the cache key because Whisper takes it as
	// recognizer-level config. The rebuild fails here (no model files), but
	// the old transcriber must still have been closed and dropped rather
	// than handed back for the wrong language.
	if _, err := c.get(spec, filepath.Join(t.TempDir(), "missing"), "en"); err == nil {
		t.Fatal("expected the rebuild to fail without model files")
	}
	if inner.closed != 1 {
		t.Fatalf("old transcriber closed %d times, want 1", inner.closed)
	}
	if c.inner != nil {
		t.Fatal("cache must not keep a transcriber it already closed")
	}
}

func TestTranscriberCacheRebuildsOnModelChange(t *testing.T) {
	inner := &fakeTranscriber{}
	c := &transcriberCache{
		spec:     asr.ModelSpec{Family: "whisper", Variant: "large-v3"},
		language: "auto",
		inner:    inner,
	}

	other := asr.ModelSpec{Family: "parakeet", Variant: "tdt-0.6b-v3"}
	if _, err := c.get(other, filepath.Join(t.TempDir(), "missing"), "auto"); err == nil {
		t.Fatal("expected the rebuild to fail without model files")
	}
	if inner.closed != 1 {
		t.Fatalf("old transcriber closed %d times, want 1", inner.closed)
	}
}

func TestTranscriberCacheEmptyCacheReportsLoadError(t *testing.T) {
	var c transcriberCache
	spec := asr.ModelSpec{Family: "whisper", Variant: "large-v3", Files: []asr.ModelFile{{Filename: "encoder.onnx"}}}
	_, err := c.get(spec, filepath.Join(t.TempDir(), "missing"), "auto")
	if err == nil {
		t.Fatal("expected an error for a model directory that does not exist")
	}
	if c.inner != nil {
		t.Fatal("a failed load must not be cached")
	}
	// warm() swallows that same error by design -- the real one surfaces from
	// get() on the dictation path.
	c.warm(spec, filepath.Join(t.TempDir(), "missing"), "auto")
	if c.inner != nil {
		t.Fatal("warm must not cache a failed load either")
	}
}

func TestLabelSpeakersLabelsBothSides(t *testing.T) {
	got := labelSpeakers("нагадати про рахунок", "давай перенесемо на завтра")
	want := "You: нагадати про рахунок\n\nCall: давай перенесемо на завтра"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLabelSpeakersOneSidedTakesStayBare(t *testing.T) {
	// A heading over a single block is noise: with nothing to contrast it
	// against, "You:" only gets in the way of pasting the text somewhere.
	if got := labelSpeakers("тільки я говорив", ""); got != "тільки я говорив" {
		t.Errorf("mic-only take got %q, want the text bare", got)
	}
	if got := labelSpeakers("", "тільки дзвінок"); got != "тільки дзвінок" {
		t.Errorf("system-only take got %q, want the text bare", got)
	}
}

func TestLabelSpeakersEmptyWhenNeitherSideDecoded(t *testing.T) {
	// The caller treats "" as "nothing was said" and skips output and
	// history entirely; labels alone must never make an empty take look
	// like a transcript.
	if got := labelSpeakers("", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestSeparateSpeakersThresholdsAreSane(t *testing.T) {
	// The point of the feature is that the toggle alone does not split a
	// take: the system side has to have actually carried sound. A zero
	// threshold would split every dictation made with the tap left on.
	if systemVoicedThreshold <= 0 {
		t.Error("a zero level threshold counts silence as speech")
	}
	if minVoicedSeconds <= 0 {
		t.Error("a zero duration threshold splits on a single notification chime")
	}
}

func TestRenderTurnsInterleavesBothChannelsByTime(t *testing.T) {
	// The whole point of the feature: what the user said belongs between the
	// replies it came between, not in a block above them.
	turns := []turn{
		{start: 0, speaker: youSpeaker, audio: []float32{1}},
		{start: 8, speaker: youSpeaker, audio: []float32{3}},
		{start: 4, speaker: 7, audio: []float32{2}},
		{start: 12, speaker: 3, audio: []float32{4}},
	}
	texts := map[float32]string{1: "питання", 2: "відповідь", 3: "уточнення", 4: "згода"}

	got := renderTurns(turns, func(a []float32) string { return texts[a[0]] })
	want := "You: питання\nSpeaker 1: відповідь\nYou: уточнення\nSpeaker 2: згода"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTurnsNumbersCallSpeakersByFirstAppearance(t *testing.T) {
	// Cluster ids are opaque and need not start at 0 or arrive in order; the
	// transcript should still open at "Speaker 1".
	turns := []turn{
		{start: 0, speaker: 9, audio: []float32{1}},
		{start: 1, speaker: 4, audio: []float32{1}},
		{start: 2, speaker: 9, audio: []float32{1}},
	}
	got := renderTurns(turns, func([]float32) string { return "x" })
	want := "Speaker 1: x\nSpeaker 2: x\nSpeaker 1: x"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderTurnsSkipsTurnsThatDecodedToNothing(t *testing.T) {
	// A stretch the recognizer found no words in must leave no empty label
	// behind, and must not consume a speaker number either.
	turns := []turn{
		{start: 0, speaker: 5, audio: []float32{1}},
		{start: 1, speaker: 6, audio: []float32{2}},
	}
	got := renderTurns(turns, func(a []float32) string {
		if a[0] == 1 {
			return ""
		}
		return "чутно"
	})
	if got != "Speaker 1: чутно" {
		t.Fatalf("got %q", got)
	}
}

func TestChannelTurnsForcesTheMicToOneSpeaker(t *testing.T) {
	// Far-end voices bleed into the microphone through the speakers, so the
	// diarizer finds several "people" there. None of them is anyone but the
	// user: the mic channel is by definition whoever is holding it.
	segments := []diarize.Segment{
		{Start: 0, End: 1, Speaker: 0},
		{Start: 2, End: 3, Speaker: 1},
	}
	turns := channelTurns(segments, make([]float32, 16000*4), youSpeaker, false)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	for _, tn := range turns {
		if tn.speaker != youSpeaker {
			t.Errorf("turn at %.0fs has speaker %d, want the mic's own", tn.start, tn.speaker)
		}
	}
}

func TestChannelTurnsKeepsCallSpeakersApart(t *testing.T) {
	segments := []diarize.Segment{
		{Start: 0, End: 1, Speaker: 0},
		{Start: 2, End: 3, Speaker: 1},
	}
	turns := channelTurns(segments, make([]float32, 16000*4), 0, true)
	if len(turns) != 2 || turns[0].speaker == turns[1].speaker {
		t.Fatalf("got %+v, want two distinct speakers", turns)
	}
}

func TestChannelTurnsDropsSegmentsPastTheAudio(t *testing.T) {
	// The segmentation model works on its own frame grid and can report a
	// range the buffer does not cover; an empty slice would panic the
	// recognizer, which indexes samples[0] with no length check.
	turns := channelTurns([]diarize.Segment{{Start: 10, End: 11}}, make([]float32, 16000), youSpeaker, false)
	if len(turns) != 0 {
		t.Fatalf("got %+v, want no turns", turns)
	}
}

func TestCountSpeakers(t *testing.T) {
	if got := countSpeakers(nil); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	got := countSpeakers([]diarize.Segment{{Speaker: 4}, {Speaker: 4}, {Speaker: 9}})
	if got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func newTestApp(t *testing.T, keepDictationAudio bool) *app {
	t.Helper()
	dir := t.TempDir()
	store := settings.NewStore(filepath.Join(dir, "settings.json"))
	cfg := store.Get()
	cfg.KeepDictationAudio = keepDictationAudio
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	return &app{store: store, hist: history.NewStore(dir)}
}

func TestSaveDictationAudioWritesBesideMeetingRecordings(t *testing.T) {
	a := newTestApp(t, true)
	at := time.Date(2026, 8, 18, 12, 30, 0, 0, time.Local)
	samples := make([]float32, 1600) // 0.1s, enough for a real WAV header check

	path := a.saveDictationAudio(at, samples)
	if path == "" {
		t.Fatal("got empty path, want the audio saved")
	}
	want := filepath.Join(a.hist.Dir(), meetingsDirName)
	if dir := filepath.Dir(path); dir != want {
		t.Errorf("saved to %s, want it beside the meeting recordings in %s", dir, want)
	}
	if base := filepath.Base(path); base != "2026-08-18-123000-dictation.wav" {
		t.Errorf("got filename %q, want the meeting-style timestamp stamp", base)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saveDictationAudio reported %q but it is not on disk: %v", path, err)
	}
}

// A take whose audio cannot be saved must never take the transcript down
// with it -- saveDictationAudio only ever returns "" on failure, and never
// panics or blocks the caller from writing the history entry regardless.
func TestSaveDictationAudioSkippedWhenSettingIsOff(t *testing.T) {
	a := newTestApp(t, false)
	path := a.saveDictationAudio(time.Now(), make([]float32, 1600))
	if path != "" {
		t.Errorf("got %q, want no file written when KeepDictationAudio is off", path)
	}
}

// The take with the worst transcript -- nothing decoded at all -- is exactly
// the one worth being able to check against its own recording (see the doc
// comment on settings.KeepDictationAudio). A prior version returned before
// ever appending a history entry or saving the audio when text was "";
// recordDictationResult must still do both.
func TestRecordDictationResultKeepsEmptyTextAndItsAudio(t *testing.T) {
	a := newTestApp(t, true)
	cfg := a.store.Get()
	at := time.Now()
	samples := make([]float32, 1600)

	a.recordDictationResult(cfg, "", at, at, 1.0, samples)

	entries, err := a.hist.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 appended even for an empty decode", len(entries))
	}
	if entries[0].Text != "" {
		t.Errorf("got Text %q, want empty", entries[0].Text)
	}
	if entries[0].AudioPath == "" {
		t.Error("got no AudioPath, want the take's audio saved even though nothing decoded")
	}
	if _, err := os.Stat(entries[0].AudioPath); err != nil {
		t.Errorf("AudioPath %q is not on disk: %v", entries[0].AudioPath, err)
	}
}

// KeepDictationAudio still governs whether the audio is written -- an empty
// decode does not override the user's own setting, it just stops overriding
// it wrongly.
func TestRecordDictationResultRespectsKeepDictationAudioOffOnEmptyText(t *testing.T) {
	a := newTestApp(t, false)
	cfg := a.store.Get()
	at := time.Now()

	a.recordDictationResult(cfg, "", at, at, 1.0, make([]float32, 1600))

	entries, err := a.hist.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].AudioPath != "" {
		t.Errorf("got AudioPath %q, want none: KeepDictationAudio is off", entries[0].AudioPath)
	}
}

// The ordinary case -- real text -- must keep behaving exactly as before:
// emitted (ModeNone here just records to history, so nothing touches the
// clipboard in a test) and appended, with its audio saved.
func TestRecordDictationResultStillHandlesNonEmptyText(t *testing.T) {
	a := newTestApp(t, true)
	cfg := a.store.Get()
	cfg.OutputMode = output.ModeNone
	at := time.Now()

	a.recordDictationResult(cfg, "hello world", at, at, 1.0, make([]float32, 1600))

	entries, err := a.hist.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "hello world" {
		t.Fatalf("got %+v, want one entry with the decoded text", entries)
	}
	if entries[0].AudioPath == "" {
		t.Error("got no AudioPath for a normal take")
	}
}

// "Paste, unless it is a task": a take the LLM made tasks of is not pasted;
// one with no tasks -- or no answer, which leaves found empty -- is.
func TestDictationOutputModeSkipsPasteForATask(t *testing.T) {
	task := []llm.Result{{Text: "call the accountant"}}
	if got := dictationOutputMode(output.ModePasteUnlessTask, task); got != output.ModeNone {
		t.Errorf("a task was delivered as %q, want %q", got, output.ModeNone)
	}
	if got := dictationOutputMode(output.ModePasteUnlessTask, nil); got != output.ModePasteUnlessTask {
		t.Errorf("a non-task was delivered as %q, want it pasted", got)
	}
	if got := dictationOutputMode(output.ModePaste, task); got != output.ModePaste {
		t.Errorf("plain paste changed to %q because of a task", got)
	}
}

func TestSegmentAtFindsTheCoveringSegment(t *testing.T) {
	segments := []diarize.Segment{
		{Start: 0, End: 2, Speaker: 0},
		{Start: 5, End: 8, Speaker: 1},
	}
	if got := segmentAt(segments, 1); got != 0 {
		t.Errorf("inside the first segment: got %d, want 0", got)
	}
	if got := segmentAt(segments, 6); got != 1 {
		t.Errorf("inside the second segment: got %d, want 1", got)
	}
	// A word in the gap the segmentation model called silence still has to be
	// attributed to somebody: the nearer neighbour.
	if got := segmentAt(segments, 2.4); got != 0 {
		t.Errorf("just after the first segment: got %d, want 0", got)
	}
	if got := segmentAt(segments, 4.6); got != 1 {
		t.Errorf("just before the second segment: got %d, want 1", got)
	}
	if got := segmentAt(nil, 1); got != -1 {
		t.Errorf("no segments: got %d, want -1", got)
	}
}

func TestWordTurnsGroupsWordsBySegment(t *testing.T) {
	segments := []diarize.Segment{
		{Start: 0, End: 2, Speaker: 4},
		{Start: 5, End: 8, Speaker: 9},
	}
	words := []asr.Word{
		{Text: "добрий", Start: 0.2},
		{Text: "день", Start: 0.8},
		{Text: "так", Start: 5.5},
		{Text: "згоден", Start: 6.0},
	}
	turns := wordTurns(words, segments, 0, true)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	if turns[0].text != "добрий день" || turns[0].speaker != 4 || turns[0].start != 0.2 {
		t.Errorf("first turn = %+v", turns[0])
	}
	if turns[1].text != "так згоден" || turns[1].speaker != 9 {
		t.Errorf("second turn = %+v", turns[1])
	}
}

func TestWordTurnsSplitsTheMicByPauseNotBySpeaker(t *testing.T) {
	// Every mic turn is the same speaker, so grouping by speaker alone would
	// collapse the user's whole side into one turn placed at the first word --
	// and the interleaving with the call would be gone.
	segments := []diarize.Segment{
		{Start: 0, End: 1, Speaker: 0},
		{Start: 9, End: 10, Speaker: 0},
	}
	words := []asr.Word{{Text: "привіт", Start: 0.1}, {Text: "дякую", Start: 9.2}}
	turns := wordTurns(words, segments, youSpeaker, false)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	for _, tn := range turns {
		if tn.speaker != youSpeaker {
			t.Errorf("turn at %.1fs has speaker %d, want the mic's own", tn.start, tn.speaker)
		}
	}
	if turns[1].start != 9.2 {
		t.Errorf("second turn starts at %v, want 9.2", turns[1].start)
	}
}

func TestWordTurnsStitchesOneSpeakerAcrossABreath(t *testing.T) {
	// The segmenter cuts on breath as well as on speaker, so one person
	// answering a question came back as several labelled lines in a row --
	// a crowd taking turns, rather than somebody talking.
	segments := []diarize.Segment{
		{Start: 0, End: 1, Speaker: 3},
		{Start: 1.6, End: 4, Speaker: 3},
	}
	words := []asr.Word{
		{Text: "так", Start: 0.2},
		{Text: "звичайно", Start: 1.8},
	}
	turns := wordTurns(words, segments, 0, true)
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1: %+v", len(turns), turns)
	}
	if turns[0].text != "так звичайно" {
		t.Errorf("stitched turn = %q", turns[0].text)
	}
	if turns[0].end < 1.8 {
		t.Errorf("stitched turn ends at %v, before its own last word", turns[0].end)
	}
}

func TestStitchTurnsLeavesARealPauseAlone(t *testing.T) {
	// Longer than maxSpeakerGap is a pause somebody else can speak into, and
	// joining across it would pile the other channel's replies after a turn
	// that had already ended.
	turns := stitchTurns([]turn{
		{start: 0, end: 1, speaker: 3, text: "питання"},
		{start: 9, end: 10, speaker: 3, text: "ще одне"},
	}, maxSpeakerGap)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
}

func TestStitchTurnsKeepsTheTwoChannelsApart(t *testing.T) {
	// The mic and the call are two recordings of the same seconds: back to
	// back in time means they overlapped, not that one followed the other.
	turns := stitchTurns([]turn{
		{start: 0, end: 1, speaker: youSpeaker, channel: channelMic, text: "привіт"},
		{start: 1.1, end: 2, speaker: youSpeaker, channel: channelSystem, text: "вітаю"},
	}, maxSpeakerGap)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
}

func TestRenderTurnsUsesTextThatIsAlreadyDecoded(t *testing.T) {
	// The word-labelling path has no audio to hand a decoder, and must not
	// need one.
	turns := []turn{
		{start: 0, speaker: youSpeaker, text: "питання"},
		{start: 4, speaker: 7, text: "відповідь"},
	}
	got := renderTurns(turns, nil)
	want := "You: питання\nSpeaker 1: відповідь"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A model can be listed for download with no engine behind it: the catalog
// here and the switch in asr are separate lists, and the only sign of the
// mistake is every dictation failing after a 700MB download.
func TestKnownModelsAllHaveAnEngine(t *testing.T) {
	for _, m := range knownModels {
		if !asr.HasEngine(m.Family) {
			t.Errorf("%s/%s is offered for download, but asr has no engine for family %q", m.Family, m.Variant, m.Family)
		}
	}
}

// A task the classifier could not place, out of a meeting that has been
// placed, belongs to the meeting's project -- Unfiltered is for a task with
// nothing to inherit from.
func TestTaskFromAMeetingInheritsTheMeetingsProject(t *testing.T) {
	dir := t.TempDir()
	a := &app{meetings: history.NewMeetingStore(dir)}
	start := time.Date(2026, 9, 23, 14, 0, 0, 0, time.Local)
	if err := a.meetings.Append(history.Meeting{Start: start, RecordingSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := a.meetings.SetEntity(start, "Northwind"); err != nil {
		t.Fatal(err)
	}
	key := start.Format(time.RFC3339Nano)

	if got := a.taskEntity(history.KindMeeting, key, llm.UnfilteredEntity); got != "Northwind" {
		t.Errorf("unplaced task got %q, want the meeting's Northwind", got)
	}
	// The classifier's own answer is the better one: it read the sentence the
	// task came out of, not the whole call.
	if got := a.taskEntity(history.KindMeeting, key, "Contoso"); got != "Contoso" {
		t.Errorf("placed task got %q, want Contoso", got)
	}
	// A dictation has no meeting to inherit from, and neither does a meeting
	// nothing has filed yet.
	if got := a.taskEntity(history.KindDictation, key, llm.UnfilteredEntity); got != llm.UnfilteredEntity {
		t.Errorf("dictation got %q, want Unfiltered", got)
	}
	unfiled := start.Add(time.Hour)
	if err := a.meetings.Append(history.Meeting{Start: unfiled, RecordingSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	got := a.taskEntity(history.KindMeeting, unfiled.Format(time.RFC3339Nano), llm.UnfilteredEntity)
	if got != llm.UnfilteredEntity {
		t.Errorf("task from an unfiled meeting got %q, want Unfiltered", got)
	}
}
