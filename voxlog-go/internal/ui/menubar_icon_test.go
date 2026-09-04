package ui

import (
	"bytes"
	"image"
	"image/color"
	_ "image/png"
	"testing"
)

// decodeGlyph decodes one of the embedded menu bar bitmaps and reports the
// shape's dimensions along with a pixel taken from the middle of the tallest
// bar, which is where the glyph's color actually lives.
func decodeGlyph(t *testing.T, data []byte) (image.Image, color.NRGBA, int, int) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("format is %q, want png -- systray hands the bytes to NSImage", format)
	}

	b := img.Bounds()
	var transparent, inked int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a == 0 {
				transparent++
			} else if a == 0xffff {
				inked++
			}
		}
	}

	r, g, bl, a := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	if a == 0 {
		t.Fatal("the center of the image is transparent; the tallest bar should cover it")
	}
	center := color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)}
	return img, center, transparent, inked
}

// Every one of these is drawn by macOS from its ALPHA channel when installed
// as a template image, so an opaque bitmap renders as a solid rectangle in
// the menu bar. That is exactly what shipped once, when these were rasterized
// with qlmanage -- which flattens transparency onto white. Nothing about the
// files looks wrong until they are on screen.
func TestMenuBarIconsHaveRealTransparency(t *testing.T) {
	for name, data := range map[string][]byte{
		"idle":         MenuBarIcon,
		"recording":    MenuBarIconRecording,
		"transcribing": MenuBarIconTranscribing,
	} {
		img, _, transparent, inked := decodeGlyph(t, data)
		b := img.Bounds()
		if b.Dx() != 32 || b.Dy() != 32 {
			t.Errorf("%s: size is %dx%d, want 32x32 (systray draws it at 16pt, so this is the @2x bitmap)", name, b.Dx(), b.Dy())
		}
		if transparent == 0 {
			t.Errorf("%s: no transparent pixels, so it would draw as a solid rectangle", name)
		}
		if inked == 0 {
			t.Errorf("%s: no opaque pixels, so the glyph would be invisible", name)
		}
		for _, p := range []image.Point{
			{b.Min.X, b.Min.Y}, {b.Max.X - 1, b.Min.Y},
			{b.Min.X, b.Max.Y - 1}, {b.Max.X - 1, b.Max.Y - 1},
		} {
			if _, _, _, a := img.At(p.X, p.Y).RGBA(); a != 0 {
				t.Errorf("%s: corner %v has alpha %d, want 0", name, p, a>>8)
			}
		}
	}
}

// The recording and transcribing glyphs are installed with SetIcon (not
// SetTemplateIcon) precisely so their color survives. If either were
// regenerated as plain black, the status item would still change -- to
// something indistinguishable from idle, which is worse than no indicator at
// all, because it looks like the feature works.
func TestBusyMenuBarIconsAreDistinctlyColored(t *testing.T) {
	_, idle, _, _ := decodeGlyph(t, MenuBarIcon)
	_, recording, _, _ := decodeGlyph(t, MenuBarIconRecording)
	_, transcribing, _, _ := decodeGlyph(t, MenuBarIconTranscribing)

	if idle.R != 0 || idle.G != 0 || idle.B != 0 {
		t.Errorf("idle glyph is %v, want black: it is installed as a template image, whose pixels macOS replaces anyway", idle)
	}
	if recording.R < 0x80 || recording.R <= recording.B {
		t.Errorf("recording glyph is %v, want a red the user can recognize", recording)
	}
	if transcribing.B < 0x80 || transcribing.B <= transcribing.R {
		t.Errorf("transcribing glyph is %v, want a blue the user can recognize", transcribing)
	}
	if recording == transcribing {
		t.Error("recording and transcribing glyphs are the same color; the two states would be indistinguishable")
	}
}
