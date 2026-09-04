package output

import (
	"os/exec"
	"strings"
	"testing"
)

// Emit's paste modes post a real Cmd+V into whatever app has focus, so those
// paths stay out of the test suite; what is covered here is the clipboard
// itself and the guard that decides whether to touch it at all.

// keepClipboard saves the pasteboard's text and puts it back when the test
// ends, so running the suite does not cost the developer their clipboard.
func keepClipboard(t *testing.T) {
	t.Helper()
	previous, hadText := ReadClipboard()
	t.Cleanup(func() {
		if hadText {
			WriteClipboard(previous)
		}
	})
}

// The transcript that reaches the pasteboard must be the transcript that was
// recognized. The first implementation shelled out to pbcopy, which has no
// encoding argument and decodes stdin using the environment's locale -- an
// app launched from Finder has no LANG, so every Cyrillic transcript arrived
// in the target app as MacRoman mojibake while the history file, written
// straight to JSON, showed the correct text. osascript reads the pasteboard
// through an independent path, which is the point: it sees what another app
// would see, not what our own writer happens to round-trip.
func TestClipboardKeepsNonASCIIText(t *testing.T) {
	keepClipboard(t)

	const text = "Привіт, світ — тест 123 ✓"
	WriteClipboard(text)

	got, ok := ReadClipboard()
	if !ok {
		t.Fatal("pasteboard reports no text right after writing some")
	}
	if got != text {
		t.Fatalf("read back %q, want %q", got, text)
	}

	out, err := exec.Command("osascript", "-e", "the clipboard as text").Output()
	if err != nil {
		t.Skipf("osascript unavailable: %v", err)
	}
	if viaOSA := strings.TrimRight(string(out), "\n"); viaOSA != text {
		t.Fatalf("another app would see %q, want %q", viaOSA, text)
	}
}

func TestEmitCopyPutsTextOnTheClipboard(t *testing.T) {
	keepClipboard(t)

	const text = "розшифрований текст"
	if err := Emit(text, ModeCopy); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got, _ := ReadClipboard(); got != text {
		t.Fatalf("clipboard holds %q, want %q", got, text)
	}
}

func TestEmitEmptyTextLeavesClipboardAlone(t *testing.T) {
	keepClipboard(t)

	const sentinel = "не чіпай мене"
	WriteClipboard(sentinel)
	for _, mode := range []string{ModeCopy, ModePaste, ModePasteCopy, ModeNone} {
		if err := Emit("", mode); err != nil {
			t.Fatalf("Emit(\"\", %q): %v", mode, err)
		}
	}
	if got, _ := ReadClipboard(); got != sentinel {
		t.Fatalf("clipboard became %q, want %q -- empty text must be a no-op", got, sentinel)
	}
}

func TestEmitModeNoneLeavesClipboardAlone(t *testing.T) {
	keepClipboard(t)

	const sentinel = "не чіпай мене"
	WriteClipboard(sentinel)
	if err := Emit("some transcript", ModeNone); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got, _ := ReadClipboard(); got != sentinel {
		t.Fatalf("clipboard became %q, want %q -- ModeNone is history-only", got, sentinel)
	}
}

func TestOutputModeConstantsMatchSettingsVocabulary(t *testing.T) {
	// Settings stores these strings verbatim (OutputMode, HistoryClickAction);
	// renaming a constant without migrating stored settings silently sends
	// every existing user down the default branch.
	cases := map[string]string{
		ModeNone:      "none",
		ModeCopy:      "copy",
		ModePaste:     "paste",
		ModePasteCopy: "paste_copy",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant is %q, want %q", got, want)
		}
	}
}
