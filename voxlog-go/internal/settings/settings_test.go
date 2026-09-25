package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"voxlog-go/internal/hotkey"
)

func TestNewStoreLoadsDefaultsWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "settings.json"))
	got := s.Get()
	want := DefaultSettings()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want defaults %+v", got, want)
	}
}

// Cleanup is opt-in. Nothing about installing this app should start deleting
// a user's recordings.
func TestCleanupIsOffByDefault(t *testing.T) {
	d := DefaultSettings()
	if d.AudioRetention != "disabled" {
		t.Errorf("AudioRetention is %q, want disabled", d.AudioRetention)
	}
	if d.AudioMaxGB != 0 {
		t.Errorf("AudioMaxGB is %v, want 0 meaning no ceiling", d.AudioMaxGB)
	}
}

func TestStoreSetPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := NewStore(path)
	v := s.Get()
	v.Language = "uk"
	v.MicGain = 1.5
	if err := s.Set(v); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s2 := NewStore(path)
	got := s2.Get()
	if got.Language != "uk" || got.MicGain != 1.5 {
		t.Fatalf("got %+v, want language=uk mic_gain=1.5", got)
	}
}

func TestStoreLoadCorruptFileFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if got := s.Get(); !reflect.DeepEqual(got, DefaultSettings()) {
		t.Fatalf("got %+v, want defaults", got)
	}
}

func TestStoreLoadPartialFileFillsMissingFieldsFromDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"language":"en"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	got := s.Get()
	want := DefaultSettings()
	want.Language = "en"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDefaultsMatchTheShippedBehaviour(t *testing.T) {
	d := DefaultSettings()
	// Escape throwing away a take is opt-in: it gets pressed constantly for
	// unrelated reasons, and losing a recording to a stray press is worse
	// than having no cancel key.
	if d.EscapeCancels {
		t.Error("EscapeCancels should default off")
	}
	// The menu bar already reports decoding; keeping the capsule up as well
	// is opt-in.
	if d.ShowTranscribingOverlay {
		t.Error("ShowTranscribingOverlay should default off")
	}
	// Unity gain leaves a normal voice quiet enough that word endings get
	// dropped.
	if d.MicGain <= 1.0 {
		t.Errorf("MicGain is %v, want more than unity", d.MicGain)
	}
	// Meetings are off until a key is bound for them: an unbound feature
	// cannot surprise anyone who did not ask for it.
	if d.MeetingKeyID != "" {
		t.Errorf("MeetingKeyID is %q, want unbound by default", d.MeetingKeyID)
	}
	// Hold-to-talk is the new mode; the key must keep behaving as it always
	// has until the user says otherwise.
	if d.DictateActivation != ActivationToggle {
		t.Errorf("DictateActivation is %q, want %q", d.DictateActivation, ActivationToggle)
	}
}

func TestExistingSettingsFileGetsTheNewDefaults(t *testing.T) {
	// A settings.json written by the current shipped version: no meeting
	// fields at all. It must load with meetings unbound and dictation
	// unchanged, not with empty strings that mean nothing downstream.
	path := filepath.Join(t.TempDir(), "settings.json")
	old := `{"language":"auto","model_family":"parakeet","model_variant":"tdt-0.6b-v3",
	         "dictate_key":"vk:54","history_key":"vk:60","output_mode":"paste",
	         "mic_gain":2,"capture_system_audio":true,"separate_speakers":true}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	got := NewStore(path).Get()
	if got.DictateActivation != ActivationToggle {
		t.Errorf("DictateActivation is %q, want %q", got.DictateActivation, ActivationToggle)
	}
	if got.MeetingTranscribe != MeetingTranscribeStop {
		t.Errorf("MeetingTranscribe is %q, want %q", got.MeetingTranscribe, MeetingTranscribeStop)
	}
	if !got.KeepMeetingAudio {
		t.Errorf("KeepMeetingAudio is %v, want true", got.KeepMeetingAudio)
	}
	// And the settings that only moved sections in the UI keep their values.
	if !got.CaptureSystemAudio || !got.SeparateSpeakers {
		t.Errorf("got %+v, want system audio and speaker separation still enabled", got)
	}
	if got.DictateKeyID != "vk:54" {
		t.Errorf("DictateKeyID is %q, want the stored binding", got.DictateKeyID)
	}
}

// Keeping the audio is the default now: a transcript is not a substitute for
// hearing what was actually said, and nothing deletes a recording until the
// user asks for that in phase 4's cleanup settings.
func TestAudioIsKeptByDefault(t *testing.T) {
	d := DefaultSettings()
	if !d.KeepDictationAudio || !d.KeepMeetingAudio {
		t.Fatalf("got dictation=%v meeting=%v, want both true", d.KeepDictationAudio, d.KeepMeetingAudio)
	}
}

// A settings file written before this change carries meeting_keep_audio.
// "never" was the one value that expressed a real refusal, so it is the one
// that survives as a false; the other two meant "keep it, at least for now".
func TestLegacyMeetingKeepAudioNeverBecomesOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"meeting_keep_audio":"never"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(path).Get(); got.KeepMeetingAudio {
		t.Error("an explicit refusal to keep meeting audio was not carried over")
	}
}

func TestLegacyMeetingKeepAudioUntilTranscribedBecomesOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"meeting_keep_audio":"until_transcribed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(path).Get(); !got.KeepMeetingAudio {
		t.Error("a settings file that meant 'keep it for now' should keep it")
	}
}

func TestEntityDictionarySeedingFlagRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := NewStore(path)
	v := s.Get()
	if v.EntityDictionarySeeded {
		t.Fatal("a fresh settings file must not claim the dictionary was seeded")
	}
	v.EntityDictionary = []string{"Voxlog"}
	v.EntityDictionarySeeded = true
	if err := s.Set(v); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got := NewStore(path).Get()
	if !got.EntityDictionarySeeded {
		t.Fatal("EntityDictionarySeeded did not survive a reload")
	}
	if len(got.EntityDictionary) != 1 || got.EntityDictionary[0] != "Voxlog" {
		t.Fatalf("EntityDictionary = %v, want [Voxlog]", got.EntityDictionary)
	}
}

func TestTapDefaultMigratesToAlways(t *testing.T) {
	// A file written before settings_version existed holds the old default,
	// which cannot notice a call the user listens to in silence. It moves on;
	// the version stamp stops it from happening twice.
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"always_on_system_audio":"session"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := NewStore(path).Get()
	if got.AlwaysOnSystemAudio != AlwaysOnTapAlways {
		t.Fatalf("always_on_system_audio = %q, want %q", got.AlwaysOnSystemAudio, AlwaysOnTapAlways)
	}
	if got.Version != currentSettingsVersion {
		t.Fatalf("Version = %d, want %d", got.Version, currentSettingsVersion)
	}
}

func TestTapChoiceSurvivesOnceTheFileIsVersioned(t *testing.T) {
	// Having migrated once, "only while recording" is a choice again and must
	// not be overridden on every launch.
	path := filepath.Join(t.TempDir(), "settings.json")
	s := NewStore(path)
	v := s.Get()
	v.AlwaysOnSystemAudio = AlwaysOnTapSession
	if err := s.Set(v); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(path).Get(); got.AlwaysOnSystemAudio != AlwaysOnTapSession {
		t.Fatalf("always_on_system_audio = %q, want the stored choice %q", got.AlwaysOnSystemAudio, AlwaysOnTapSession)
	}
}

// Summarizing used to be implied by task_hub_enabled, so a settings file
// written before these fields existed must still summarize: reading a
// missing summary_enabled as "off" would turn the feature off for everyone
// who already had it.
func TestSummarizingStaysOnForAFileThatPredatesIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"task_hub_enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := NewStore(path).Get()
	if !got.SummaryEnabled {
		t.Error("SummaryEnabled is off for a file written before it existed")
	}
	if got.SummaryLength != SummaryNormal {
		t.Errorf("SummaryLength = %q, want %q", got.SummaryLength, SummaryNormal)
	}
	if got.SummaryPromptExtra != "" {
		t.Errorf("SummaryPromptExtra = %q, want empty", got.SummaryPromptExtra)
	}
}

// The quick-tasks drawer is the only way to write a task down by hand, so it
// ships bound: Command-/ on a fresh install, and on a file that predates the
// default too. The binding has to be one hotkey.ParseBinding accepts -- a
// typo here would leave the drawer silently unreachable.
func TestTasksKeyIsBoundToCommandSlashByDefault(t *testing.T) {
	if got := DefaultSettings().TasksKeyID; got != DefaultTasksKeyID {
		t.Fatalf("TasksKeyID = %q, want %q", got, DefaultTasksKeyID)
	}
	b := hotkey.ParseBinding(DefaultTasksKeyID)
	if b.IsZero() {
		t.Fatalf("ParseBinding(%q) is zero -- the default binding never fires", DefaultTasksKeyID)
	}
	if !reflect.DeepEqual(b.Mods, []string{hotkey.ModCommand}) {
		t.Errorf("modifiers = %v, want [%s]", b.Mods, hotkey.ModCommand)
	}
	if b.Label() != "Command + /" {
		t.Errorf("Label() = %q, want %q", b.Label(), "Command + /")
	}
}

func TestEmptyTasksKeyMigratesToTheDefault(t *testing.T) {
	// Every install written before the default existed carries an empty
	// tasks_key that nobody chose. It moves on; the version stamp stops it
	// from happening twice.
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"tasks_key":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := NewStore(path).Get()
	if got.TasksKeyID != DefaultTasksKeyID {
		t.Fatalf("tasks_key = %q, want %q", got.TasksKeyID, DefaultTasksKeyID)
	}
	if got.Version != currentSettingsVersion {
		t.Fatalf("Version = %d, want %d", got.Version, currentSettingsVersion)
	}
}

func TestAChosenTasksKeySurvivesTheMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"tasks_key":"ctrl+vk:8"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(path).Get(); got.TasksKeyID != "ctrl+vk:8" {
		t.Fatalf("tasks_key = %q, want the stored choice %q", got.TasksKeyID, "ctrl+vk:8")
	}
}
