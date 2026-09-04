package sfsymbol

import (
	"bytes"
	"image/png"
	"testing"
)

// The menu falls back to a bare title when a symbol is missing, so the thing
// worth checking is that the ones it actually asks for are present and decode
// as images -- a silently empty icon is the failure mode this catches.
func TestSymbolsUsedByTheMenuRender(t *testing.T) {
	for _, name := range []string{
		"record.circle", "stop.circle", "mic", "mic.slash", "waveform",
	} {
		data := PNG(name, 16)
		if len(data) == 0 {
			t.Errorf("%s: no PNG", name)
			continue
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestUnknownSymbolIsNilNotEmptyImage(t *testing.T) {
	if got := PNG("definitely.not.a.symbol", 16); got != nil {
		t.Errorf("got %d bytes, want nil so the caller can skip the icon", len(got))
	}
}

// Tinting must not silently produce the same bitmap as the template render --
// the menu bar's muted glyph is coloured precisely so it reads as a warning.
func TestTintedDiffersFromTemplate(t *testing.T) {
	plain, tinted := PNG("mic.slash", 18), TintedPNG("mic.slash", 18, 0xE0, 0x3E, 0x3E)
	if len(plain) == 0 || len(tinted) == 0 {
		t.Skip("no mic.slash on this macOS")
	}
	if bytes.Equal(plain, tinted) {
		t.Error("the tinted glyph came back identical to the template one")
	}
}
