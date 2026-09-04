package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Settings struct {
	// Language is "auto", "uk", or "en". Only honored by models whose
	// ModelSpec.SupportsLanguage is true (currently Nemotron); the others
	// auto-detect with no pinning API, so the Settings window forces this
	// to "auto" for them rather than offering a control that does nothing.
	Language     string `json:"language"`
	ModelFamily  string `json:"model_family"`  // "whisper" | "parakeet" | "nemotron"
	ModelVariant string `json:"model_variant"` // e.g. "base.en", "tdt-0.6b-v3"
	DictateKeyID string `json:"dictate_key"`   // "vk:54" / "sym:shift_r" form
	HistoryKeyID string `json:"history_key"`
	InputDevice  string `json:"input_device"` // capture device name; "" = system default
	// OutputMode is what happens to a finished transcript:
	// "none" | "copy" | "paste" | "paste_copy" (see internal/output).
	OutputMode string  `json:"output_mode"`
	MicGain    float64 `json:"mic_gain"`
	// LiveStreamingText only applies to models with SupportsStreaming.
	LiveStreamingText bool `json:"live_streaming_text"`
	// EscapeCancels lets the Escape key throw away an in-progress recording.
	// Off by default: Escape is pressed constantly for unrelated reasons, and
	// losing a take to a stray press is worse than having no cancel key.
	EscapeCancels bool `json:"escape_cancels"`
	// ShowTranscribingOverlay keeps the capsule on screen after recording
	// stops, showing that decoding is still running. Off by default: it draws
	// the eye to a window that has nothing left to say.
	ShowTranscribingOverlay bool `json:"show_transcribing_overlay"`
	// IndicatorStyle picks the recording indicator's layout (see
	// internal/ui's IndicatorStyles). The style owns where the indicator
	// appears -- "cuff" and "island" hang off the notch, the two
	// teleprompters sit at the bottom of the screen -- so OverlayPosition
	// only still applies to "bars", the capsule this app shipped with.
	IndicatorStyle string `json:"indicator_style"`
	// The five switches below decide which parts of the indicator are
	// mounted. Every style is the same markup with parts turned on and off,
	// which is why these are booleans rather than another layout each.
	//
	// IndicatorStopButton is the one with a consequence beyond looks: an
	// indicator carrying a button has to accept the mouse, so it stops being
	// click-through and starts covering whatever is under it.
	IndicatorWave       bool `json:"indicator_wave"`
	IndicatorTimer      bool `json:"indicator_timer"`
	IndicatorModeLabel  bool `json:"indicator_mode_label"`
	IndicatorStopButton bool `json:"indicator_stop_button"`
	// IndicatorSolid draws the indicator on solid black instead of glass.
	// True by default because the default style meets the notch, where solid
	// black is the whole illusion; anywhere else the same black reads as a
	// hole punched in the desktop.
	IndicatorSolid   bool `json:"indicator_solid"`
	IndicatorOutline bool `json:"indicator_outline"`
	// Size of the level meter, in points. A knob rather than a constant
	// because how wide a meter should be depends on the screen it is on and
	// on how much of it the user wants to see out of the corner of an eye.
	// Clamped on the way out (see ui.IndicatorParts) so a hand-edited file
	// cannot produce an indicator wider than its window.
	IndicatorWaveWidth  int `json:"indicator_wave_width"`
	IndicatorWaveHeight int `json:"indicator_wave_height"`
	// OverlayPosition is where the recording indicator appears:
	// "cursor" (beside the pointer, placed once per take) | "follow" (tracks
	// the pointer) | the four corners of the usable screen area | and the two
	// screen_bottom_* slots down beside the Dock (see internal/ui's
	// OverlayPositions).
	OverlayPosition string `json:"overlay_position"`
	// CaptureSystemAudio mixes whatever is playing through the speakers
	// into the recording alongside the microphone. Needs Screen Recording
	// permission (macOS treats even audio-only capture as screen capture).
	CaptureSystemAudio bool `json:"capture_system_audio"`
	// SeparateSpeakers transcribes the microphone and the system audio as
	// two streams and labels them, instead of mixing them into one. Only
	// meaningful with CaptureSystemAudio on, and only takes effect when the
	// system side actually carried sound -- a take with nothing playing has
	// exactly one speaker and nothing to separate.
	SeparateSpeakers bool `json:"separate_speakers"`
	// HistoryClickAction is what confirming an entry in the History pane
	// does -- clicking its row, or selecting it with the arrow keys and
	// pressing Enter: output.ModeNone, output.ModeCopy, or output.ModePaste,
	// which puts the window away, hands focus back to the app it was opened
	// from, and pastes there. A stored "paste_copy" is read as plain paste:
	// it dates from the popover, and paste already leaves the clipboard as
	// it found it.
	HistoryClickAction string `json:"history_click_action"`
	// HistoryRetention is how long transcripts are kept:
	// "disabled" | "week" | "two_weeks" | "month" (see internal/history).
	HistoryRetention string `json:"history_retention"`
	// TranscriptsDir is where the per-day history files live. Empty means
	// the default (~/Documents/Voxlog/Transcripts).
	TranscriptsDir string `json:"transcripts_dir"`
	// MeetingKeyID starts and stops a meeting recording -- a call captured
	// from beginning to end, rather than a dictation. Empty by default: until
	// the user binds a key, meetings do not exist and nothing about the app
	// changes.
	MeetingKeyID string `json:"meeting_key"`
	// DictateActivation is how the dictate key behaves: ActivationToggle
	// (tap to start, tap to stop) or ActivationHold (recording lasts exactly
	// as long as the key is held). Dictation only -- nobody holds a key for an
	// hour, so the meeting key is always a toggle.
	DictateActivation string `json:"dictate_activation"`
	// MeetingTranscribe is when a meeting gets decoded: MeetingTranscribeStop
	// (as soon as it ends) or MeetingTranscribeManual (never, until asked for
	// it from the History window).
	MeetingTranscribe string `json:"meeting_transcribe"`
	// KeepDictationAudio writes each dictation to disk beside its transcript,
	// so a take can be played back or run through another model later. On by
	// default: the recording is the only thing a wrong transcript can be
	// checked against.
	KeepDictationAudio bool `json:"keep_dictation_audio"`
	// KeepMeetingAudio keeps a meeting's recording after it has been
	// transcribed. On by default, and nothing deletes it until the cleanup
	// settings are turned on.
	KeepMeetingAudio bool `json:"keep_meeting_audio"`
	// AudioRetention deletes recordings past a certain age:
	// "disabled" (default) | "week" | "two_weeks" | "month". It shares its
	// vocabulary with HistoryRetention but not its value -- transcripts are
	// cheap to keep and recordings are not, so the two are set separately.
	AudioRetention string `json:"audio_retention"`
	// AudioMaxGB caps what the recordings folder may occupy, oldest first.
	// Zero, the default, means no ceiling.
	AudioMaxGB float64 `json:"audio_max_gb"`
	// BackfillTurns is when meetings recorded before per-speaker replies
	// existed get re-read for them: BackfillIdle (while nothing else is
	// going on), BackfillStartup (as soon as the app has settled), or
	// BackfillManual (only from a meeting's own Transcribe button).
	//
	// Idle by default. Re-reading hours of old audio is real CPU, and the
	// machine belongs to the user, not to the backlog.
	BackfillTurns string `json:"backfill_turns"`
	// TaskHubEnabled turns on background LLM classification of finished
	// transcripts (Settings > LLM model). Off by default: experimental,
	// downloads a multi-gigabyte model, and the checkbox itself stays
	// disabled in the UI until that model is on disk.
	TaskHubEnabled bool `json:"task_hub_enabled"`
	// EntityDictionary is the user-maintained list of canonical
	// project/client names Task Hub's classifier is told to reuse instead of
	// inventing near-duplicates. Editable in Settings > LLM model -- fixing a
	// misrecognized name here only affects future classification, nothing is
	// rewritten retroactively.
	EntityDictionary []string `json:"entity_dictionary"`
	// EntityDictionarySeeded records that the one-time seeding of
	// EntityDictionary from already-classified tasks has run. Without it the
	// Settings pane re-adds every entity it finds on every open, resurrecting
	// names the user has just deleted.
	EntityDictionarySeeded bool `json:"entity_dictionary_seeded"`
}

// Dictation activation modes.
const (
	ActivationToggle = "toggle"
	ActivationHold   = "hold"
)

// Meeting transcription timing.
const (
	MeetingTranscribeStop   = "stop"
	MeetingTranscribeManual = "manual"
)

func DefaultSettings() Settings {
	return Settings{
		Language:     "auto",
		ModelFamily:  "parakeet",
		ModelVariant: "tdt-0.6b-v3",
		DictateKeyID: "vk:54", // right Command
		HistoryKeyID: "vk:60", // right Shift
		InputDevice:  "",
		OutputMode:   "paste",
		// 2.0, not 1.0: at unity gain a normal speaking voice a normal
		// distance from a laptop mic lands quiet enough that the recognizer
		// drops word endings.
		MicGain:                 2.0,
		LiveStreamingText:       false,
		EscapeCancels:           false,
		ShowTranscribingOverlay: false,
		// The cuff hangs off the notch, where nothing of the user's lives and
		// where solid black merges with the hardware. It is also the only
		// default that says which mode is running and how to stop it.
		IndicatorStyle:      "cuff",
		IndicatorWave:       true,
		IndicatorTimer:      false,
		IndicatorModeLabel:  true,
		IndicatorStopButton: false,
		IndicatorSolid:      true,
		IndicatorOutline:    false,
		// 96, not the 148 this used to be fixed at: a pill carrying a mode
		// label and a shortcut was running most of the way across the notch.
		IndicatorWaveWidth:  96,
		IndicatorWaveHeight: 16,
		OverlayPosition:     "cursor",
		CaptureSystemAudio:  false,
		SeparateSpeakers:    false,
		HistoryClickAction:  "copy",
		HistoryRetention:    "disabled",
		TranscriptsDir:      "",
		// Unbound: a user who never wants meeting recording sees no change in
		// behavior, and no key of theirs is quietly taken over.
		MeetingKeyID:      "",
		DictateActivation: ActivationToggle,
		MeetingTranscribe: MeetingTranscribeStop,
		// Recordings accumulate on purpose now: a transcript is not a
		// substitute for hearing what was actually said, and nothing deletes
		// audio until the user turns on the cleanup settings in a later phase.
		KeepDictationAudio: true,
		KeepMeetingAudio:   true,
		// Matches history.RetentionDisabled, spelled out as a literal so this
		// package does not need to import internal/history.
		AudioRetention: "disabled",
		AudioMaxGB:     0,
		BackfillTurns:  BackfillIdle,
	}
}

// When old meetings are re-read for their per-speaker replies.
const (
	BackfillIdle    = "idle"
	BackfillStartup = "startup"
	BackfillManual  = "manual"
)

func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "Library", "Application Support", "Voxlog", "settings.json")
}

type Store struct {
	path   string
	values Settings
}

func NewStore(path string) *Store {
	if path == "" {
		path = Path()
	}
	s := &Store{path: path, values: DefaultSettings()}
	s.Load()
	return s
}

func (s *Store) Load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.values = DefaultSettings()
		return nil
	}

	merged := DefaultSettings()
	if err := json.Unmarshal(raw, &merged); err != nil {
		s.values = DefaultSettings()
		return nil
	}

	// A file written before KeepMeetingAudio existed carries the old
	// three-value meeting_keep_audio instead, which the unmarshal above
	// leaves merged.KeepMeetingAudio at its default of true. Only "never" was
	// an actual refusal to keep the audio; "always" and "until_transcribed"
	// both meant "keep it, at least for now" and are already what the default
	// gives, so "never" is the one value worth carrying forward.
	var legacy struct {
		MeetingKeepAudio string `json:"meeting_keep_audio"`
	}
	if err := json.Unmarshal(raw, &legacy); err == nil && legacy.MeetingKeepAudio == "never" {
		merged.KeepMeetingAudio = false
	}

	s.values = merged
	return nil
}

func (s *Store) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

func (s *Store) Get() Settings { return s.values }

func (s *Store) Set(v Settings) error {
	s.values = v
	return s.Save()
}
