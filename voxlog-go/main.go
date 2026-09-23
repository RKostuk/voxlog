package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
	"voxlog-go/internal/hotkey"
	"voxlog-go/internal/permissions"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/sfsymbol"
	"voxlog-go/internal/task"
	"voxlog-go/internal/ui"
	"voxlog-go/internal/usernotify"
)

// notify shows a native macOS notification banner -- the smallest way to
// surface a dictation failure the user can actually see, instead of it only
// landing in a log file nobody's watching.
func notify(message string) {
	usernotify.Post(message, "")
}

// notifyPane is notify for the banners that have somewhere to go: clicking one
// opens the main window on the named pane, the same names ShowMainWindow takes.
func notifyPane(message, pane string) {
	usernotify.Post(message, pane)
}

// knownModels lists the downloadable ASR models offered in the settings
// window. IMPORTANT: asr's offline/online engines (offline.go/online.go)
// hardcode the local filenames "encoder.onnx"/"decoder.onnx"/"joiner.onnx"/
// "tokens.txt" when loading a model directory - they do not consult
// ModelFile.Filename. So every entry below MUST use exactly those Filename
// values regardless of what the upstream repo actually calls the file; only
// the URL varies per model/variant.
var knownModels = []asr.ModelSpec{
	{
		Family:  "whisper",
		Variant: "large-v3",
		// The accuracy option. Multilingual (not an .en build), and Whisper
		// takes the language as recognizer config, so it can be pinned to
		// Ukrainian or English rather than always guessing.
		SupportsLanguage:  true,
		SupportsStreaming: false,
		Description:       "Best accuracy, slowest \u00b7 ~1.8 GB",
		Files: []asr.ModelFile{
			// int8 builds specifically: the plain large-v3-encoder.onnx in
			// this repo is a graph-only stub that needs a 3GB .weights
			// sidecar, while these int8 files are self-contained.
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-whisper-large-v3/resolve/main/large-v3-encoder.int8.onnx", Filename: "encoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-whisper-large-v3/resolve/main/large-v3-decoder.int8.onnx", Filename: "decoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-whisper-large-v3/resolve/main/large-v3-tokens.txt", Filename: "tokens.txt"},
		},
	},
	{
		Family:  "nemotron",
		Variant: "streaming-320ms",
		// The only streaming engine here, and the only one with a real
		// language prompt: its encoder takes an extra prompt_index input
		// that conditions decoding on a locale (see online.go).
		SupportsLanguage:  true,
		SupportsStreaming: true,
		Description:       "Live text while you speak \u00b7 lower accuracy \u00b7 ~0.7 GB",
		Files: []asr.ModelFile{
			// sherpa-onnx's OWN export, not the generic ONNX one: the
			// generic exports (pantinor, onnx-community) ship a combined
			// decoder_joint.onnx plus tokenizer.model, which sherpa-onnx
			// cannot load at all -- it wants separate encoder/decoder/joiner
			// and a tokens.txt, which is exactly what this repo provides.
			// 320ms chunk, int8: the middle latency option, ~685MB total.
			{URL: "https://huggingface.co/csukuangfj2/sherpa-onnx-nemotron-3.5-asr-streaming-0.6b-320ms-int8-2026-06-11/resolve/main/encoder.int8.onnx", Filename: "encoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj2/sherpa-onnx-nemotron-3.5-asr-streaming-0.6b-320ms-int8-2026-06-11/resolve/main/decoder.int8.onnx", Filename: "decoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj2/sherpa-onnx-nemotron-3.5-asr-streaming-0.6b-320ms-int8-2026-06-11/resolve/main/joiner.int8.onnx", Filename: "joiner.onnx"},
			{URL: "https://huggingface.co/csukuangfj2/sherpa-onnx-nemotron-3.5-asr-streaming-0.6b-320ms-int8-2026-06-11/resolve/main/tokens.txt", Filename: "tokens.txt"},
		},
	},
	{
		Family:  "parakeet",
		Variant: "tdt-0.6b-v3",
		// Auto-detect only, batch decode: no language pinning, no live text.
		SupportsLanguage:  false,
		SupportsStreaming: false,
		Description:       "Best for dictation \u00b7 25 languages \u00b7 ~2.5 GB",
		Files: []asr.ModelFile{
			// Confirmed against the real repo file listing: encoder.onnx,
			// decoder.onnx, joiner.onnx, tokens.txt are the actual filenames.
			// encoder.onnx stores its weights externally (ONNX external-data
			// mechanism) in a separate ~2.4GB encoder.weights file that MUST
			// sit alongside it in the same directory under that exact name --
			// found the hard way: sherpa-onnx aborts the process (not a Go
			// error, a C++ uncaught exception -> SIGABRT) if it's missing,
			// since ValidateExternalDataPathFromDir has no graceful failure.
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3/resolve/main/encoder.onnx", Filename: "encoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3/resolve/main/encoder.weights", Filename: "encoder.weights"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3/resolve/main/decoder.onnx", Filename: "decoder.onnx"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3/resolve/main/joiner.onnx", Filename: "joiner.onnx"},
			{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3/resolve/main/tokens.txt", Filename: "tokens.txt"},
		},
	},
}

// expandHome resolves a leading "~/" in a user-typed path. Settings holds
// paths as the user entered them, and a literal "~" directory is never what
// they meant.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}

func mustUserConfigDir() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(dir, "Library", "Application Support")
}

// setupLogging mirrors the log to ~/Library/Logs/Voxlog.log. A menu bar app
// launched from Finder has nowhere to print: stdout goes to the void, so
// without this every diagnostic the app writes is simply lost.
func setupLogging() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, "Library", "Logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "Voxlog.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	// Point the process's own fd 2 at the log file, unconditionally.
	//
	// This is what makes a crash readable at all: the Go runtime writes panic
	// tracebacks straight to fd 2, bypassing the log package entirely, and
	// sherpa-onnx's C++ side reports its complaints the same way. Launched
	// from Finder fd 2 is /dev/null, so a panic killed Voxlog leaving no
	// crash report, no log line, and nothing to go on.
	//
	// Unconditional rather than only-when-not-a-terminal: /dev/null is a
	// character device too, so "is stderr a terminal?" answered yes for
	// exactly the launch mode that needed the redirect. Running from a
	// terminal now prints nothing and needs a `tail -f` on the log, which is
	// a smaller price than losing crashes in the shipped app.
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		log.Printf("could not redirect stderr to the log: %v", err)
		return
	}
	// One destination now: stderr IS the log file.
	log.SetOutput(os.Stderr)
}

func main() {
	setupLogging()
	log.Printf("--- Voxlog starting ---")
	store := settings.NewStore("")
	// NewStore("") falls back to its own default dir, so an unset
	// TranscriptsDir needs no special case here.
	histStore := history.NewStore(expandHome(store.Get().TranscriptsDir))
	// Meetings live beside the day files rather than inside them: a few a
	// day, each with two audio tracks and a length measured in hours, is not
	// the shape a per-day list of dictated sentences is good at.
	//
	// Their records live in a database rather than in files, because a
	// meeting now carries turns, speakers and voice fingerprints -- see
	// internal/history/db.go. The database sits beside the settings, NOT in
	// the transcripts directory: that one is user-settable and routinely
	// points into iCloud or Dropbox, and SQLite over a syncing filesystem is
	// a well-known way to lose a database. The recordings and the frozen JSON
	// backup stay where the user's own backups already cover them.
	meetingsDir := filepath.Join(histStore.Dir(), "Meetings")
	meetDB, err := history.OpenDB(filepath.Join(mustUserConfigDir(), "Voxlog", "voxlog.db"))
	if err != nil {
		log.Fatalf("opening the meetings database: %v", err)
	}
	meetStore := history.NewMeetingStoreDB(meetingsDir, meetDB)
	taskStore := task.NewStore("")
	modelsDir := filepath.Join(mustUserConfigDir(), "Voxlog", "models")

	systray.Run(func() {
		onReady(store, histStore, meetStore, taskStore, modelsDir)
	}, func() {})
}

// transcriberCache holds one loaded ASR model between dictations. Loading a
// model reads gigabytes off disk (parakeet's encoder.weights alone is 2.4GB)
// and measured ~4s in practice, so building a fresh transcriber per dictation
// -- as this originally did -- made every hotkey tap feel broken: recording
// didn't actually begin until the load finished, and an impatient second tap
// arriving during the load stopped a recording that had captured nothing.
type transcriberCache struct {
	mu       sync.Mutex
	spec     asr.ModelSpec
	language string
	inner    asr.Transcriber
}

// get returns a transcriber for spec, reusing the cached one whenever the
// selected model hasn't changed. Concurrent callers block on mu, which is
// what lets the dictation-stop path simply wait for an in-flight background
// warm() of the same model instead of racing it into a second load.
func (c *transcriberCache) get(spec asr.ModelSpec, modelDir, language string) (asr.Transcriber, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Language is part of the key, not just the model: Whisper takes it as
	// recognizer-level config, so a language change needs a rebuild rather
	// than a per-call argument.
	if c.inner != nil && c.spec.Family == spec.Family && c.spec.Variant == spec.Variant && c.language == language {
		return c.inner, nil
	}
	if c.inner != nil {
		c.inner.Close() // model or language switched in Settings; drop the old one
		c.inner = nil
	}

	t, err := asr.NewTranscriber(spec, modelDir, language)
	if err != nil {
		return nil, err
	}
	c.spec, c.language, c.inner = spec, language, t
	return t, nil
}

// warm loads spec's model if it isn't already cached, discarding any error
// -- callers use this purely to move the load cost off the critical path;
// the real error surfaces from get() when the transcript is actually needed.
func (c *transcriberCache) warm(spec asr.ModelSpec, modelDir, language string) {
	if _, err := c.get(spec, modelDir, language); err != nil {
		log.Printf("model preload: %v", err)
	}
}

// diarizerCache holds the loaded speaker models, and owns the one-time
// download of them.
//
// The models are fetched on first need rather than up front: they are ~35MB
// that most users never want, since they only matter with system audio and
// the speaker toggle both on. The take that triggers the download does NOT
// wait for it -- it falls back to a single "Call:" block, and the next one
// gets speakers. Blocking a dictation on a 35MB transfer would read as the
// app having hung.
type diarizerCache struct {
	mu          sync.Mutex
	inner       *diarize.Diarizer
	downloading bool
}

// get returns the diarizer, or nil if the models are not on disk yet (having
// started fetching them in the background).
func (c *diarizerCache) get(modelsDir string) *diarize.Diarizer {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inner != nil {
		return c.inner
	}

	if !asr.IsDownloaded(modelsDir, diarize.Spec) {
		if !c.downloading {
			c.downloading = true
			go func() {
				log.Printf("diarization: downloading speaker models")
				err := asr.Download(modelsDir, diarize.Spec, func(string, int64, int64) {}, fetchURL)
				c.mu.Lock()
				c.downloading = false
				c.mu.Unlock()
				if err != nil {
					log.Printf("diarization: download failed: %v", err)
					notify("Could not download the speaker models.")
					return
				}
				log.Printf("diarization: models ready")
			}()
		}
		return nil
	}

	d, err := diarize.New(asr.ModelDir(modelsDir, diarize.Spec), 4)
	if err != nil {
		log.Printf("diarization: %v", err)
		return nil
	}
	c.inner = d
	return d
}

// fetchURL is asr.Download's transport. Kept here rather than shared with the
// settings window's copy because that one reports progress into a webview.
func fetchURL(url string) (io.ReadCloser, int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

// mixAudio sums two mono streams into one, sample by sample, clipped to
// the [-1, 1] range the recognizer expects. Length mismatches are normal --
// the mic and the system tap start and stop microseconds apart and run off
// different clocks -- so the result is as long as the longer of the two and
// the shorter one simply contributes nothing past its end.
//
// ponytail: no drift correction or alignment. Both streams are nominally
// 16kHz and a dictation take is seconds long, so accumulated drift stays
// far below anything a speech model would notice.
func mixAudio(a, b []float32) []float32 {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}

	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var sum float32
		if i < len(a) {
			sum += a[i]
		}
		if i < len(b) {
			sum += b[i]
		}
		if sum > 1 {
			sum = 1
		} else if sum < -1 {
			sum = -1
		}
		out[i] = sum
	}
	return out
}

// Labels used when the two streams are transcribed apart. Deliberately not
// "Speaker 1/2": the split here is by source, not by voice, and the app knows
// exactly which source is which.
const (
	micLabel    = "You:"
	systemLabel = "Call:"
)

// systemVoicedThreshold is the audio.Level a system-audio chunk has to reach
// to count as sound rather than room tone or a hiss floor. audio.Level scales
// RMS by 8 and clamps at 1, which puts speech around 0.1-1.0 and an idle
// output stream near zero.
const systemVoicedThreshold = 0.05

// minVoicedSeconds is how much sound the system side needs before the take is
// treated as having a second speaker at all. A notification chime or a single
// UI click is not a conversation, and splitting a plain dictation in two
// costs a second decode pass and puts a "Call:" heading on nothing.
const minVoicedSeconds = 0.5

// Diarization tuning. maxSpeakerGap is how long one person may pause before
// the next stretch counts as a new turn rather than the same one; below
// minSpeakerSegment a stretch is too short to decode into anything but junk.
//
// 1.5s rather than 1.0: a turn broken in two costs a label either way, and
// on the word-labelling path it costs nothing at all, while on the fallback
// path every extra break is another sliver handed to the recognizer.
const (
	maxSpeakerGap     = 1.5
	minSpeakerSegment = 0.6
)

// segmentPad widens a segment before its audio is cut out, on the fallback
// path that still cuts. The segmentation model marks where a voice is, not
// where a word begins: cut on the mark exactly and the first consonant and
// the last vowel are left outside the slice, which is a large part of why
// per-segment decoding reads like a bad phone line. The padding is allowed to
// overlap the neighbouring turn -- a word heard twice is a smaller error than
// half a word heard once.
const segmentPad = 0.25

// youSpeaker marks a turn as the microphone's, i.e. the user's own. A
// negative id can never collide with a cluster id from the diarizer.
const youSpeaker = -1

// wordTail is how long the last word of a turn is assumed to run for, when
// the recognizer reports where each word started but not where it ended.
const wordTail = 0.4

// A meeting is two recordings, not one: the microphone and the system audio
// are separate files (see meeting.go), so every turn has to say which.
const (
	channelMic    = 0
	channelSystem = 1
)

// turn is one stretch of one voice, from whichever channel it came in on.
//
// Exactly one of text and audio is set, which is the difference between the
// two ways a conversation can be transcribed: text means the words are
// already decoded and were sorted into speakers afterwards (wordTurns), audio
// means this stretch still has to be decoded on its own (channelTurns).
type turn struct {
	start float32
	// end is when the voice stopped, in the same clock as start. Storing a
	// turn without it would make "play this reply" mean "play from here to
	// the end of the call".
	end     float32
	speaker int
	// channel says which of a meeting's two recordings this turn is in --
	// they are separate files, so a reply cannot be played back without it.
	channel int
	text    string
	audio   []float32
	// block is which decoded block this turn came out of. Speaker numbers are
	// only comparable within one block (see speakers.go), so the block has to
	// travel with the turn until they have been linked.
	block int
	// embed is this turn's voice fingerprint, filled in only for the far end
	// of a call, where telling speakers apart is the open question. Never
	// persisted from here: it feeds the per-meeting clustering in voiceid.
	embed []float32
}

// channelTurns cuts one channel into turns. The call side keeps the
// diarizer's speaker ids so several people can be told apart; the mic side is
// forced to youSpeaker, because who is holding the microphone is not in
// question, and any far-end voice bleeding into it through the speakers must
// not become a speaker of its own.
func channelTurns(segments []diarize.Segment, samples []float32, speaker int, keepSpeakers bool) []turn {
	turns := make([]turn, 0, len(segments))
	for _, s := range segments {
		padded := s
		padded.Start -= segmentPad
		padded.End += segmentPad
		if padded.Start < 0 {
			padded.Start = 0
		}
		slice := diarize.Slice(samples, padded, audio.SampleRate)
		if len(slice) == 0 {
			continue
		}
		id := speaker
		if keepSpeakers {
			id = s.Speaker
		}
		// The padding is a decode trick -- it gives the recognizer a run-up --
		// not a claim about when the voice started, so the turn keeps the
		// segment's own bounds.
		ch := channelMic
		if keepSpeakers {
			ch = channelSystem
		}
		turns = append(turns, turn{start: s.Start, end: s.End, speaker: id, channel: ch, audio: slice})
	}
	return turns
}

// segmentAt says which segment covers time t: the one it falls inside, or
// failing that the nearest one, since a word can land in a gap the
// segmentation model called silence and still has to be attributed to
// somebody. Returns -1 only when there are no segments at all. Segments are
// in start order, and there are a handful of them per block.
//
// ponytail: linear scan, binary search if a block ever holds thousands.
func segmentAt(segments []diarize.Segment, t float32) int {
	best, bestGap := -1, float32(0)
	for i, s := range segments {
		if t >= s.Start && t <= s.End {
			return i
		}
		gap := s.Start - t
		if gap < 0 {
			gap = t - s.End
		}
		if best == -1 || gap < bestGap {
			best, bestGap = i, gap
		}
	}
	return best
}

// wordTurns splits an already-decoded channel into turns by when each word
// was spoken. This is the path that keeps the recognizer's quality: it saw the
// whole block in one pass, with all the context that gives it, and the only
// thing being cut here is the text.
//
// Words are grouped by the segment they fall in, not by speaker id, so one
// person speaking twice with a real pause between still produces two turns --
// which is what lets the microphone's turns interleave with the call's
// instead of piling up as one block at whatever moment the user first spoke.
func wordTurns(words []asr.Word, segments []diarize.Segment, speaker int, keepSpeakers bool) []turn {
	turns := make([]turn, 0, len(segments)+1)
	last := -2
	for _, w := range words {
		if w.Text == "" {
			continue
		}
		at := segmentAt(segments, w.Start)
		id := speaker
		if keepSpeakers && at >= 0 {
			id = segments[at].Speaker
		}
		// A word carries a start and no length, so a turn's end is its last
		// word plus a mouthful -- clamped to the segment the words came from,
		// which is the only real evidence of when the voice stopped.
		end := w.Start + wordTail
		if at >= 0 && end > segments[at].End {
			end = segments[at].End
		}
		ch := channelMic
		if keepSpeakers {
			ch = channelSystem
		}
		if n := len(turns); n > 0 && at == last && turns[n-1].speaker == id {
			turns[n-1].text += " " + w.Text
			if end > turns[n-1].end {
				turns[n-1].end = end
			}
			continue
		}
		turns = append(turns, turn{start: w.Start, end: end, speaker: id, channel: ch, text: w.Text})
		last = at
	}
	return turns
}

// renderTurns decodes the turns in time order and labels each line. This is
// where the two channels become one conversation: the mic's turns and the
// call's are sorted together by when they happened, so what the user said
// sits between the replies it came between, rather than in a block of its own
// above them.
//
// The two streams start within milliseconds of each other (both are begun by
// the same hotkey tap) so their clocks are directly comparable.
func renderTurns(turns []turn, decode func([]float32) string) string {
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].start < turns[j].start })

	// Cluster ids are opaque and need not start at 0 or arrive in order, so
	// speakers are numbered by when they first say something.
	number := map[int]int{}
	var b strings.Builder
	for _, t := range turns {
		// A turn that already carries its text was decoded as part of a whole
		// channel and only sorted into speakers afterwards; decode is for the
		// turns that are still audio. See turn.
		text := t.text
		if text == "" && decode != nil {
			text = decode(t.audio)
		}
		if text == "" {
			continue
		}
		label := micLabel
		if t.speaker != youSpeaker {
			n, ok := number[t.speaker]
			if !ok {
				n = len(number) + 1
				number[t.speaker] = n
			}
			label = fmt.Sprintf("Speaker %d:", n)
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s %s", label, text)
	}
	return b.String()
}

// countSpeakers reports how many distinct speakers the segments cover.
func countSpeakers(segments []diarize.Segment) int {
	seen := map[int]bool{}
	for _, s := range segments {
		seen[s.Speaker] = true
	}
	return len(seen)
}

// labelSpeakers renders the two transcripts as one labeled block. Either side
// may be empty -- the mic side when a take was only about what was playing,
// the system side when the recognizer found no words in it -- and an empty
// side contributes no heading, so the result never carries a label with
// nothing under it. With only one side present the text is returned bare,
// exactly as an unsplit take would have produced it.
//
// ponytail: the two sides are whole blocks, not interleaved turns. Ordering
// them properly needs per-token timestamps from both passes, which sherpa
// does return -- worth doing if takes get long enough that block order stops
// reading as a conversation.
func labelSpeakers(mic, system string) string {
	switch {
	case mic == "" && system == "":
		return ""
	case system == "":
		return mic
	case mic == "":
		return system
	default:
		return micLabel + " " + mic + "\n\n" + systemLabel + " " + system
	}
}

// needsSetup reports whether the app is unusable as configured, and so
// should show Settings on its own rather than sit silently in the menu bar.
// Deliberately narrow: only the things that make dictation impossible --
// no usable model downloaded, or no hotkey to start one with. Anything the
// user could reasonably leave at its default is not "setup".
func needsSetup(cfg settings.Settings, modelsDir string) bool {
	if cfg.DictateKeyID == "" {
		return true
	}
	spec, ok := modelForSettings(cfg)
	return !ok || !asr.IsDownloaded(modelsDir, spec)
}

// modelForSettings resolves the configured family/variant to its full
// ModelSpec (with the Files list the ASR engine needs).
func modelForSettings(cfg settings.Settings) (asr.ModelSpec, bool) {
	for _, spec := range knownModels {
		if spec.Family == cfg.ModelFamily && spec.Variant == cfg.ModelVariant {
			return spec, true
		}
	}
	return asr.ModelSpec{}, false
}

// toggleTasksDrawer opens or closes the quick-tasks drawer, reading the
// placement from settings each time rather than at startup: unlike the
// hotkey itself, where the drawer opens can change without a restart.
func toggleTasksDrawer(store *settings.Store, tasks *task.Store) {
	ui.SetDrawerPlacement(store.Get().TasksDrawerPlacement)
	ui.ToggleTasksDrawer(tasks)
}

// bindingsFor reads the four hotkeys out of the settings, refusing a meeting
// or tasks key that collides with one already taken. A binding that means two things
// would simply do the second one, with nothing on screen saying why -- the
// Settings window rejects the collision when it is set, and this covers a
// settings file edited by hand.
func bindingsFor(cfg settings.Settings) (dictate, hist, meeting, tasks hotkey.Binding) {
	dictate = hotkey.ParseBinding(cfg.DictateKeyID)
	hist = hotkey.ParseBinding(cfg.HistoryKeyID)
	meeting = hotkey.ParseBinding(cfg.MeetingKeyID)
	tasks = hotkey.ParseBinding(cfg.TasksKeyID)
	// Always-on listening owns the microphone and decides for itself when a
	// conversation is happening. A key that starts a second recorder behind
	// that gate can only produce two recordings of one room, so it is not
	// registered at all while listening is on.
	if cfg.AlwaysOn && !meeting.IsZero() {
		log.Print("meeting key ignored: always-on listening records conversations by itself")
		meeting = hotkey.Binding{}
	}
	if !meeting.IsZero() && (meeting.String() == dictate.String() || meeting.String() == hist.String()) {
		log.Printf("meeting key %s is already bound elsewhere; ignoring it", meeting.Label())
		meeting = hotkey.Binding{}
	}
	if !tasks.IsZero() && (tasks.String() == dictate.String() || tasks.String() == hist.String() ||
		(!meeting.IsZero() && tasks.String() == meeting.String())) {
		log.Printf("tasks key %s is already bound elsewhere; ignoring it", tasks.Label())
		tasks = hotkey.Binding{}
	}
	return dictate, hist, meeting, tasks
}

// muteMicLabel is the mute item's two faces. It says what the click will do,
// not what the state is -- the state is on the line above it and in the menu
// bar itself.
func muteMicLabel(muted bool) string {
	if muted {
		return "Unmute my microphone"
	}
	return "Mute my microphone"
}

// setSymbol gives a menu item an SF Symbol as its template icon, and leaves it
// with no icon at all where the symbol is missing -- an item with a blank
// square in front of it looks broken in a way a plain title does not.
func setSymbol(item *systray.MenuItem, name string) {
	if png := sfsymbol.PNG(name, menuSymbolPoints); png != nil {
		item.SetTemplateIcon(png, png)
	}
}

// menuSymbolPoints matches the size AppKit draws menu item images at; asking
// for the symbol at that size means macOS's own optical variant rather than a
// scaled-down big one.
const menuSymbolPoints = 14

// blinkMeetingStatus keeps the recording line alive: a dot that comes and
// goes, and the elapsed time beside it. A static "Recording" line is
// indistinguishable from a stale menu; a blink is the cheapest thing that
// reads as "happening now".
//
// Every title change goes through ui.RunOnMain for the reason tray.go
// documents at length: systray marshals to the main thread on its own, and
// mixing that with this app's dispatch_async route deadlocked it.
// what is a running recording, as the status line reports it: the label to
// put in front of the clock, how long it has been going, and -- for a meeting
// -- whether the microphone is muted, which is too important to leave to the
// wording of the item below it.
func blinkStatus(item *systray.MenuItem, stop <-chan struct{}, label string, state func() (bool, time.Duration, bool)) {
	const blinkInterval = 700 * time.Millisecond
	ticker := time.NewTicker(blinkInterval)
	defer ticker.Stop()

	on := true
	for {
		running, elapsed, muted := state()
		if !running {
			return
		}
		dot := "○"
		if on {
			dot = "●"
		}
		title := dot + "  " + label + " " + clockLabel(elapsed)
		if muted {
			title += "  —  mic muted"
		}
		ui.RunOnMain(func() { item.SetTitle(title) })
		on = !on

		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func onReady(store *settings.Store, histStore *history.Store, meetStore *history.MeetingStore, taskStore *task.Store, modelsDir string) {
	// Icon, not a title: a text "V" sat in the menu bar looking like a stray
	// letter next to every other app's glyph.
	a := newApp(store, histStore, meetStore, taskStore, modelsDir, ui.NewOverlay())
	a.tray.refresh()

	// Both dictation and meeting audio land here (see meetingsDirName); it is
	// the one directory the main window's loopback server is allowed to read
	// recordings from.
	recordingsDir := filepath.Join(histStore.Dir(), meetingsDirName)

	// Init/SetHandler go up here, before the hotkey listener below starts
	// accepting presses (it used to run after, leaving every notification
	// fired by an early hotkey press -- e.g. "model not downloaded" on the
	// very first dictate attempt -- racing the still-unanswered permission
	// prompt). Post() itself now queues until authorization is decided (see
	// internal/usernotify), so this mainly shrinks that window further and
	// makes sure a click has somewhere to route from the first notification
	// on.
	usernotify.Init()
	usernotify.SetHandler(func(pane string) {
		ui.ShowMainWindow(pane, histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
	})

	// The recorder section comes first and is fenced off by its own heading
	// and separator: what a menu bar recorder is for is starting and stopping
	// a recording, and that had been sitting below three window-opening items.
	//
	// Glyphs rather than image icons -- one emoji in the title costs nothing,
	// where a real template icon per item means four more assets through
	// tools/mkicons for the same "which item is this" glance.
	// ponytail: swap in template icons if the glyphs ever look out of place.
	mMeetingHeader := systray.AddMenuItem("Meeting recorder", "")
	mMeetingHeader.Disable() // a heading, not an action
	// The blinking dot (see blinkStatus) is the only thing in the menu that
	// says a recording is running RIGHT NOW rather than merely possible, and
	// it is where a muted microphone is spelled out.
	mMeetingStatus := systray.AddMenuItem("", "")
	mMeetingStatus.Disable()
	mMeetingStatus.Hide()
	mDictationStatus := systray.AddMenuItem("", "")
	mDictationStatus.Disable()
	mDictationStatus.Hide()

	mStartMeeting := systray.AddMenuItem("Start meeting", "Record a call, both sides")
	setSymbol(mStartMeeting, "record.circle")
	mMuteMic := systray.AddMenuItem(muteMicLabel(false), "Silence your side for the rest of this recording")
	setSymbol(mMuteMic, "mic.slash")
	mMuteMic.Hide()
	mStopMeeting := systray.AddMenuItem("Stop meeting recording", "End the meeting being recorded")
	setSymbol(mStopMeeting, "stop.circle")
	mStopMeeting.Hide() // only meaningful while one is running

	mStartDictation := systray.AddMenuItem("Start dictation", "Dictate and paste the transcript")
	setSymbol(mStartDictation, "mic")
	mStopDictation := systray.AddMenuItem("Stop dictation", "Finish the take and transcribe it")
	setSymbol(mStopDictation, "stop.circle")
	mStopDictation.Hide()
	systray.AddSeparator()

	// Same four panes, same order, as the sidebar in the window itself
	// (see assets/main.html's #shell-nav) -- each opens the window straight
	// onto that pane rather than always onto whichever one it last showed.
	mOverview := systray.AddMenuItem("Overview", "Open Voxlog")
	mHistory := systray.AddMenuItem("History", "Open history")
	mMeetings := systray.AddMenuItem("Meetings", "Open meetings")
	mTasksDrawer := systray.AddMenuItem("Quick tasks", "Open the tasks drawer")
	// Always-on's pause. Shown only while the feature is on: a menu item
	// that pauses something not happening says nothing.
	mListenPause := systray.AddMenuItem(listenPauseLabel(false), listenPauseTip)
	// Beside it, the softer switch: keep listening, stop writing me down.
	// Shown and hidden with the pause item, since neither means anything
	// while the feature is off.
	mListenMute := systray.AddMenuItem(listenMuteLabel(false), listenMuteTip)
	setSymbol(mListenMute, "mic.slash")
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings", "Open settings")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit Voxlog")

	// applyMeetingItems draws the meeting section of the menu.
	//
	// While always-on listening is on there is no meeting section at all:
	// the app decides for itself when a conversation is happening, so an
	// item that starts one by hand is a second way to do the thing that is
	// already being done -- and two recorders on one microphone is the bug
	// it invites. The Meetings pane stays; only the recorder goes.
	applyMeetingItems := func(recording bool) {
		listening := a.store.Get().AlwaysOn
		ui.RunOnMain(func() {
			if listening {
				mMeetingHeader.Hide()
				mMeetingStatus.Hide()
				mStartMeeting.Hide()
				mMuteMic.Hide()
				mStopMeeting.Hide()
				return
			}
			mMeetingHeader.Show()
			if recording {
				mMeetingStatus.Show()
				mMuteMic.SetTitle(muteMicLabel(false)) // this recording starts unmuted
				mMuteMic.Show()
				mStopMeeting.Show()
				mStartMeeting.Hide()
				return
			}
			mMeetingStatus.Hide()
			mMuteMic.Hide()
			mStopMeeting.Hide()
			mStartMeeting.Show()
		})
	}

	stopMeetingBlink := make(chan struct{})
	a.onMeetingState = func(running bool) {
		applyMeetingItems(running)
		if running {
			stopMeetingBlink = make(chan struct{})
			go blinkStatus(mMeetingStatus, stopMeetingBlink, "Meeting", func() (bool, time.Duration, bool) {
				running, elapsed := a.meetingElapsed()
				// Either mute counts here. A recording opened by always-on
				// has no meeting mute of its own, and the status line saying
				// nothing about it is exactly the silent state §12 is for.
				return running, elapsed, a.meetingMuted() || a.micMuted()
			})
			return
		}
		close(stopMeetingBlink)
	}

	// A dictation gets the same treatment: the menu is the only way to stop
	// one when the hotkey is unavailable, and it was the one running state the
	// menu said nothing about.
	stopDictationBlink := make(chan struct{})
	a.tray.onRecording = func(running bool) {
		ui.RunOnMain(func() {
			if running {
				mDictationStatus.Show()
				mStopDictation.Show()
				mStartDictation.Hide()
				return
			}
			mDictationStatus.Hide()
			mStopDictation.Hide()
			mStartDictation.Show()
		})
		if running {
			stopDictationBlink = make(chan struct{})
			go blinkStatus(mDictationStatus, stopDictationBlink, "Dictation", func() (bool, time.Duration, bool) {
				running, elapsed := a.dictationElapsed()
				return running, elapsed, false
			})
			return
		}
		close(stopDictationBlink)
	}

	// Load the configured model now, in the background, so the first
	// dictation of the session doesn't pay the multi-second load itself.
	cfg := store.Get()
	if spec, ok := modelForSettings(cfg); ok && asr.IsDownloaded(modelsDir, spec) {
		go a.models.warm(spec, asr.ModelDir(modelsDir, spec), cfg.Language)
	}

	// Once, on the first launch after meetings got their own store. The
	// sentinel lives with the settings rather than with the transcripts, so
	// pointing the app at a different transcripts folder does not re-run it.
	// This runs before retention pruning below so an upgrade launch with
	// retention enabled can't prune away meetings before they're moved out
	// of the day files.
	sentinel := filepath.Join(mustUserConfigDir(), "Voxlog", "migrated-meetings")
	if n, err := history.MigrateMeetings(histStore, meetStore, sentinel); err != nil {
		log.Printf("migrating meetings: %v", err)
	} else if n > 0 {
		log.Printf("moved %d meetings into their own store", n)
	}

	// And once more, on the first launch after those per-meeting files became
	// database rows. Runs second because the step above may have just written
	// new JSON files this one needs to see. The files themselves are left
	// alone: they are a backup for one release, not an intermediate.
	dbSentinel := filepath.Join(mustUserConfigDir(), "Voxlog", "migrated-sqlite")
	if n, err := history.MigrateJSONMeetingsToDB(meetStore, meetStore.Dir(), dbSentinel); err != nil {
		log.Printf("importing meetings into the database: %v", err)
	} else if n > 0 {
		log.Printf("imported %d meetings into the database", n)
	}

	// Apply the retention policy once at startup. Doing it here (rather than
	// on a timer) is enough: it only ever deletes whole days that are already
	// past the cutoff, and the app is relaunched often enough that stale files
	// never linger long.
	if n, err := histStore.Prune(cfg.HistoryRetention); err != nil {
		log.Printf("history prune: %v", err)
	} else if n > 0 {
		log.Printf("history prune: removed %d expired day file(s)", n)
	}
	// A meeting's audio is on disk from its first chunk, so a crash or a quit
	// mid-call leaves a recording with no history entry. Pick those up
	// before the sweep below runs: the sweep only knows about files on disk
	// and the live meeting's own WAVs, so an orphan with no history entry
	// yet is invisible to it and would otherwise be deletable on sight. This
	// is a glob, a couple of WAV header reads, and a JSON append -- cheap
	// enough to do synchronously and be done before the sweep looks.
	a.adoptOrphanedMeetings()

	// Same timing as the prune above: once at startup, synchronously, is
	// enough for something that only ever catches up on what accumulated
	// since the last launch.
	a.sweepRecordings()

	// Meetings recorded before per-speaker replies existed still have their
	// audio, so the replies are recoverable -- see backfill.go. Idle by
	// default, so this costs nothing until the machine is free.
	a.startBackfill()

	// Transcribing a past meeting is driven from the History window, which
	// cannot reach into this package -- hand it the entry point instead.
	ui.SetTranscribeHandler(a.transcribeStoredMeeting)

	// The Overview banner polls this to show a running meeting's elapsed
	// time -- ui cannot reach a.meeting itself, the same reason
	// SetTranscribeHandler exists.
	ui.SetMeetingStatusHandler(func() (bool, float64) {
		// One question -- "is anything being recorded right now" -- answered
		// in one place, so the banner covers an always-on session as well as
		// a meeting the user started (see meetingElapsed).
		running, elapsed := a.meetingElapsed()
		return running, elapsed.Seconds()
	})
	// The banner's Stop button goes through the same method the tray menu's
	// "Stop meeting recording" item and the meeting hotkey already use.
	ui.SetStopMeetingHandler(a.stopMeetingIfRunning)

	// ui cannot call notify directly (it would import main); give it the
	// same banner every other user-visible failure in this file uses.
	ui.SetNotifier(notify)
	// Settings asks this to explain a silent far end (see noteSystemAudioFailure).
	ui.SetSystemAudioErrorFunc(SystemAudioError)

	// The Free up space button in Settings runs the same sweep this file
	// already runs at startup and after each recording -- ui cannot decide
	// for itself which WAV a live meeting still has open.
	ui.SetSweepHandler(a.sweepRecordings)

	// CGEventTapCreate can succeed and return a live port even without
	// Accessibility permission -- it just silently delivers no events
	// afterward, so the hotkeys look "broken" with no error anywhere.
	// PromptAccessibilityTrust shows macOS's own native grant dialog (a no-op
	// if already trusted); notify() is a backup for when that system dialog
	// gets dismissed or missed.
	if !hotkey.PromptAccessibilityTrust() {
		notify("Voxlog needs Accessibility permission for hotkeys to work. Grant it in System Settings, then restart Voxlog.")
	}

	// The drawer opens where the settings say, refreshes the main window
	// after an edit made in it, and hands "Open ↗" back to the full Tasks
	// pane. All three need things package ui cannot reach on its own.
	ui.SetMainRefresher(func() { ui.RefreshMainWindowIfOpen(histStore, meetStore, a.tasks) })
	ui.SetDrawerOpenMainHandler(func() {
		ui.ShowMainWindow("tasks", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
	})

	dictateKey, historyKey, meetingKey, tasksKey := bindingsFor(cfg)
	listener := hotkey.NewListener(dictateKey, historyKey, meetingKey, tasksKey, hotkey.Callbacks{
		// Every hotkey callback is invoked synchronously from the CGEventTap's
		// C callback on the OS event-tap thread, so each one hops onto its own
		// goroutine immediately: anything slow there risks macOS disabling the
		// tap. Recording start/stop is quick, but it is not the event tap's
		// thread's business to wait for even that.
		Dictate:     func() { go a.guarded(a.toggleDictate) },
		DictateDown: func() { go a.guarded(a.holdDictate) },
		DictateUp:   func() { go a.guarded(a.releaseDictate) },
		Meeting:     func() { go a.guarded(a.toggleMeeting) },
		History:     func() { go showHistory(histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir) },
		Tasks:       func() { go toggleTasksDrawer(store, a.tasks) },
		// Escape is a global event tap: it fires on every Escape key-down in
		// every app, not just when Voxlog's window is up. Only hide the
		// window when Voxlog is frontmost, or pressing Escape in some other
		// app (a text editor, a browser) yanks Voxlog's window away for a
		// reason the user has no way to connect to what they just did.
		// Throwing away a recording in progress is opt-in: Escape gets pressed
		// constantly for unrelated reasons, and silently losing a take you were
		// in the middle of is far worse than having no cancel key at all. Read
		// per press, so the setting applies without a restart.
		Escape: func() {
			if store.Get().EscapeCancels {
				go a.cancelDictate()
			}
			// On its own goroutine, not here: "is Voxlog frontmost?" is an
			// AppKit question and is answered on the main thread now, and the
			// event tap's own thread must not be the one waiting for it --
			// macOS disables a tap whose callback takes too long.
			go func() {
				if ui.AppIsActive() {
					ui.HideMainWindow()
				}
			}()
		},
		Hold: func() bool { return store.Get().DictateActivation == settings.ActivationHold },
	})
	go func() {
		if err := listener.Start(); err != nil {
			log.Printf("hotkey listener: %v", err)
		}
	}()

	log.Printf("DEBUG permissions: accessibility=%v microphone=%v screenrecording=%v",
		permissions.Accessibility(), permissions.Microphone(), permissions.ScreenRecording())

	// Open Settings unprompted in the two cases where a bare menu bar icon
	// would leave the user stuck: right after a restart they asked for from
	// that window, and on a first run where nothing is set up yet.
	openSettings := false
	for _, arg := range os.Args[1:] {
		if arg == ui.OpenSettingsFlag {
			openSettings = true
			break
		}
	}
	if !openSettings && needsSetup(cfg, modelsDir) {
		openSettings = true
	}
	if openSettings {
		go ui.ShowMainWindow("settings", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
	}

	// Reminders for tasks whose time already passed while the app was closed
	// fire once, right now, instead of being silently dropped; future ones
	// are armed normally. Must run after SetHandler above, so a reminder
	// clicked the instant it fires has somewhere to go.
	task.RescheduleAll(a.tasks)

	// The MCP server, if it is turned on. Installed before it is first
	// applied so a save that arrives while the window is open reaches the
	// same code path as this startup call.
	ui.SetSettingsAppliedFunc(a.applyMCP)
	ui.SetMCPStatusFunc(a.mcpStatus)
	ui.SetMCPRegenerateFunc(a.regenerateMCPToken)
	a.applyMCP(store.Get())

	// Always-on listening. The supervisor runs for the life of the app and
	// decides on each tick whether the microphone should be open at all --
	// the setting, the pause switch, the frontmost app and whatever else is
	// already recording all get a say (see alwayson.go).
	a.startAlwaysOn()
	// The menu follows the setting without a restart: turning listening on
	// takes the meeting recorder out of the menu, turning it off puts it
	// back. Same poll as the supervisor, so the two never disagree for more
	// than a tick.
	go func() {
		for {
			listening := a.store.Get().AlwaysOn
			ui.RunOnMain(func() {
				if listening {
					mListenPause.Show()
					mListenMute.Show()
				} else {
					mListenPause.Hide()
					mListenMute.Hide()
				}
			})
			recording, _ := a.meetingElapsed()
			applyMeetingItems(recording)
			time.Sleep(listenPollInterval)
		}
	}()

	go func() {
		for {
			select {
			case <-mOverview.ClickedCh:
				go ui.ShowMainWindow("overview", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
			case <-mHistory.ClickedCh:
				go ui.ShowMainWindow("history", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
			case <-mMeetings.ClickedCh:
				go ui.ShowMainWindow("meetings", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
			case <-mListenPause.ClickedCh:
				paused := a.toggleListenPause()
				ui.RunOnMain(func() { mListenPause.SetTitle(listenPauseLabel(paused)) })
			case <-mListenMute.ClickedCh:
				// The menu bar itself has to say this, not just the item:
				// "listening, but not to you" must never be a silent state.
				muted := a.toggleListenMute()
				a.tray.setListenMuted(muted)
				ui.RunOnMain(func() { mListenMute.SetTitle(listenMuteLabel(muted)) })
			case <-mTasksDrawer.ClickedCh:
				// Same entry point as the tasks hotkey, so a menu open and a
				// key open cannot drift apart.
				go toggleTasksDrawer(store, a.tasks)
			case <-mStartMeeting.ClickedCh:
				// The same entry point the meeting hotkey uses, so a menu
				// start and a key start cannot drift apart -- and so the
				// recorder still works on a machine where Accessibility
				// permission (and with it every hotkey) is missing.
				go a.guarded(a.toggleMeeting)
			case <-mStartDictation.ClickedCh:
				go a.guarded(a.toggleDictate)
			case <-mStopDictation.ClickedCh:
				go a.guarded(a.toggleDictate)
			case <-mMuteMic.ClickedCh:
				muted := a.toggleMeetingMute()
				a.tray.setMicMuted(muted)
				ui.RunOnMain(func() { mMuteMic.SetTitle(muteMicLabel(muted)) })
			case <-mStopMeeting.ClickedCh:
				go a.guarded(a.stopMeetingIfRunning)
			case <-mSettings.ClickedCh:
				go ui.ShowMainWindow("settings", histStore, meetStore, a.tasks, store, knownModels, modelsDir, recordingsDir)
			case <-mQuit.ClickedCh:
				// A meeting in progress is written to disk as it records, so
				// quitting cannot lose it -- but closing it properly is what
				// gives it a history entry and a correct WAV header.
				a.stopMeetingIfRunning()
				a.llm.Shutdown()
				listener.Stop()
				systray.Quit()
				return
			}
		}
	}()
}

// showHistory toggles the window on its History pane: pressing the key again
// puts it away rather than re-raising a window that is already in front of
// you. Only when Voxlog is also the app in front -- otherwise the press means
// "show me the history", never "toggle it off", and a window left open behind
// the editor would answer the hotkey by disappearing.
func showHistory(histStore *history.Store, meetStore *history.MeetingStore, tasks *task.Store, store *settings.Store, models []asr.ModelSpec, modelsDir, recordingsDir string) {
	if ui.AppIsActive() && ui.MainWindowVisible() {
		ui.HideMainWindow()
		return
	}
	ui.ShowMainWindow("history", histStore, meetStore, tasks, store, models, modelsDir, recordingsDir)
}

// guarded runs one hotkey action, surviving a panic inside it.
//
// A take runs a lot of cgo over models that can be missing, truncated, or
// simply not what their filename claims. Without this, one bad take takes the
// whole app down with it -- the menu bar icon just disappears mid-dictation,
// which is exactly how this was first reported. Log the stack (stderr lands in
// the log file, see setupLogging) and keep running.
func (a *app) guarded(action func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC during a hotkey action: %v\n%s", r, debug.Stack())
			notify("Voxlog hit an unexpected error. See ~/Library/Logs/Voxlog.log")
			a.tray.refresh()
			a.overlay.SetTranscribing(false)
			a.overlay.Hide()
		}
	}()
	action()
}
