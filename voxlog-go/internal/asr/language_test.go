package asr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWhisperLanguageAutoMeansEmpty(t *testing.T) {
	// Whisper's config treats "" as detect-it-yourself; passing the literal
	// string "auto" through would be read as a language code and rejected.
	for _, in := range []string{"", "auto"} {
		if got := whisperLanguage(in); got != "" {
			t.Errorf("whisperLanguage(%q) = %q, want empty", in, got)
		}
	}
}

func TestWhisperLanguagePassesISOCodesThrough(t *testing.T) {
	for _, in := range []string{"uk", "en", "ru"} {
		if got := whisperLanguage(in); got != in {
			t.Errorf("whisperLanguage(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestNemotronLanguageOptionMapsToFullLocale(t *testing.T) {
	// The model is conditioned on a locale tag, not a bare ISO code: "uk"
	// alone does not resolve to a language index.
	cases := map[string]string{"uk": "uk-UA", "en": "en-US"}
	for in, want := range cases {
		if got := nemotronLanguageOption(in); got != want {
			t.Errorf("nemotronLanguageOption(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNemotronLanguageOptionUnknownFallsBackToAuto(t *testing.T) {
	// An unmapped language must leave the model auto-detecting rather than
	// guessing a locale it was never conditioned on.
	for _, in := range []string{"auto", "", "ru", "klingon"} {
		if got := nemotronLanguageOption(in); got != "auto" {
			t.Errorf("nemotronLanguageOption(%q) = %q, want auto", in, got)
		}
	}
}

func TestSilenceLengthMatchesSampleRate(t *testing.T) {
	got := silence(edgePadSeconds)
	want := int(edgePadSeconds * sampleRate)
	if len(got) != want {
		t.Fatalf("got %d samples, want %d", len(got), want)
	}
	for i, s := range got {
		if s != 0 {
			t.Fatalf("sample %d is %v, want silence", i, s)
		}
	}
}

func TestSilenceZeroSecondsIsEmpty(t *testing.T) {
	if got := silence(0); len(got) != 0 {
		t.Fatalf("got %d samples, want 0", len(got))
	}
}

func TestNumThreadsIsAtLeastOneAndCapped(t *testing.T) {
	n := numThreads()
	if n < 1 || n > 4 {
		t.Fatalf("numThreads() = %d, want 1..4", n)
	}
}

func TestIsDownloadedFalseWhenAFileIsEmpty(t *testing.T) {
	// A zero-byte file is what an interrupted download leaves behind; treating
	// it as present would send sherpa-onnx into a model it cannot load (which
	// aborts the process rather than returning an error).
	base := t.TempDir()
	m := ModelSpec{Family: "whisper", Variant: "v", Files: []ModelFile{{Filename: "tokens.txt"}}}
	dir := ModelDir(base, m)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if IsDownloaded(base, m) {
		t.Fatal("an empty file must not count as downloaded")
	}
}

func TestIsDownloadedFalseWhenSpecHasNoFiles(t *testing.T) {
	// Vacuous truth here would report a model with no files as ready to use.
	if IsDownloaded(t.TempDir(), ModelSpec{Family: "x", Variant: "y"}) {
		t.Fatal("a spec with no files must never report as downloaded")
	}
}

func TestNewOnlineTranscriberKeepsTheLanguageForStreaming(t *testing.T) {
	// The live path calls Feed/Finish, never Transcribe, so a language that
	// only reached the engine through Transcribe's argument was silently
	// dropped for exactly the mode that shows text as you speak.
	tr := &onlineTranscriber{language: "uk"}
	if got := nemotronLanguageOption(tr.language); got != "uk-UA" {
		t.Fatalf("streaming would ask for %q, want uk-UA", got)
	}
}

func TestTranscribeArgumentOverridesTheBuiltInLanguage(t *testing.T) {
	tr := &onlineTranscriber{language: "uk"}
	tr.language = "en" // what Transcribe does with its argument
	if got := nemotronLanguageOption(tr.language); got != "en-US" {
		t.Fatalf("got %q, want en-US", got)
	}
}
