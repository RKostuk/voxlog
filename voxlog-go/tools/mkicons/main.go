// Command mkicons renders Voxlog's icon assets: the .iconset PNGs that
// iconutil turns into AppIcon.icns, the menu bar template glyph, and an
// optional preview PNG.
//
// This exists because the obvious approach -- draw the icon in SVG, rasterize
// it with qlmanage (the only SVG renderer macOS ships) -- produces fully
// opaque PNGs: qlmanage flattens transparency onto white. That is fatal for
// both outputs. A template image is drawn from its ALPHA channel alone, so an
// opaque one renders as a solid black rectangle in the menu bar, and an app
// icon with opaque corners shows a white square around the rounded tile.
//
// Geometry lives here rather than in an SVG so there is exactly one source
// for the shipped bitmaps; run `make icons` after changing it.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

// Colors of the tile gradient and the mark drawn on it.
var (
	gradTop    = color.NRGBA{0x63, 0x66, 0xF1, 0xff} // indigo
	gradBottom = color.NRGBA{0x8B, 0x5C, 0xF6, 0xff} // violet
	markColor  = color.NRGBA{0xff, 0xff, 0xff, 0xff}
)

// rect is a rounded rectangle in normalized coordinates: 0..1 across the
// output image, whatever pixel size it is rendered at.
type rect struct{ x, y, w, h, r float64 }

// contains reports whether (px, py), also normalized, is inside r.
func (b rect) contains(px, py float64) bool {
	// Distance from the point to the rounded rect's "inner" box, which is the
	// rect shrunk by its corner radius. Inside the inner box that distance is
	// zero on both axes; only near a corner do both components go non-zero,
	// and there the test becomes the corner circle.
	dx := math.Max(math.Max(b.x+b.r-px, px-(b.x+b.w-b.r)), 0)
	dy := math.Max(math.Max(b.y+b.r-py, py-(b.y+b.h-b.r)), 0)
	return dx*dx+dy*dy <= b.r*b.r
}

// The app icon tile. macOS app icons since Big Sur are a rounded square
// inset within the canvas rather than filling it: drawn edge to edge, the
// icon reads as oversized next to every system icon beside it.
const tileInset = 100.0 / 1024.0
const tileSize = 824.0 / 1024.0

var tile = rect{
	x: tileInset, y: tileInset, w: tileSize, h: tileSize,
	r: 184.0 / 1024.0,
}

// waveformBars returns the five-bar mark, normalized into a box of side
// `scale` offset by `off` -- so the same geometry serves the app icon (inside
// the tile) and the menu bar glyph (filling the whole image).
//
// heights are fractions of the box; the mark is centered vertically.
func waveformBars(off, scale float64, barW, gap float64, heights []float64) []rect {
	total := float64(len(heights))*barW + float64(len(heights)-1)*gap
	x := (1 - total) / 2
	bars := make([]rect, 0, len(heights))
	for _, h := range heights {
		bars = append(bars, rect{
			x: off + x*scale,
			y: off + (1-h)/2*scale,
			w: barW * scale,
			h: h * scale,
			r: barW / 2 * scale,
		})
		x += barW + gap
	}
	return bars
}

// The mark as it sits on the app icon tile: modest bars with room around them.
func iconBars() []rect {
	return waveformBars(tileInset, tileSize, 0.09, 0.07, []float64{0.23, 0.44, 0.62, 0.44, 0.23})
}

// The mark as the menu bar glyph. Taller and wider relative to its box than
// the icon version: there is no tile around it, so the glyph IS the icon, and
// at 16pt every unused pixel is one the shape cannot afford.
func glyphBars() []rect {
	return waveformBars(0, 1, 0.09375, 0.078125, []float64{0.3125, 0.5625, 0.8125, 0.5625, 0.3125})
}

// samples per axis for antialiasing; 4x4 per pixel is plenty at these sizes
// and keeps even the 16px icon's corners clean.
const samples = 4

// coverage returns what fraction of the pixel at (px, py) is inside any of
// the shapes, sampled on a samples x samples grid.
func coverage(shapes []rect, px, py, size float64) float64 {
	hits := 0
	for sy := 0; sy < samples; sy++ {
		for sx := 0; sx < samples; sx++ {
			x := (px + (float64(sx)+0.5)/samples) / size
			y := (py + (float64(sy)+0.5)/samples) / size
			for _, s := range shapes {
				if s.contains(x, y) {
					hits++
					break
				}
			}
		}
	}
	return float64(hits) / float64(samples*samples)
}

// blend composites src over dst with src's alpha, in straight (not
// premultiplied) 8-bit sRGB -- the same arithmetic the SVG preview used.
func blend(dst, src color.NRGBA, alpha float64) color.NRGBA {
	if alpha <= 0 {
		return dst
	}
	mix := func(d, s uint8) uint8 {
		return uint8(math.Round(float64(d)*(1-alpha) + float64(s)*alpha))
	}
	return color.NRGBA{mix(dst.R, src.R), mix(dst.G, src.G), mix(dst.B, src.B), dst.A}
}

// renderIcon draws the app icon at size x size pixels: gradient tile,
// a light-to-dark sheen down its face, the white mark, and -- crucially --
// transparent pixels everywhere outside the tile.
func renderIcon(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	fsize := float64(size)
	bars := iconBars()
	tileShape := []rect{tile}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			a := coverage(tileShape, float64(x), float64(y), fsize)
			if a == 0 {
				continue // outside the tile: leave it fully transparent
			}

			// Diagonal gradient, top-left to bottom-right.
			t := (float64(x)/fsize + float64(y)/fsize) / 2
			c := color.NRGBA{
				lerp(gradTop.R, gradBottom.R, t),
				lerp(gradTop.G, gradBottom.G, t),
				lerp(gradTop.B, gradBottom.B, t),
				255,
			}

			// Sheen: a white wash over the top half fading out by the middle,
			// then a slight darkening toward the bottom. Gives the flat tile
			// the same subtle roundness the system icons have.
			v := float64(y) / fsize
			if v < 0.5 {
				c = blend(c, color.NRGBA{255, 255, 255, 255}, 0.22-0.20*(v/0.5))
			} else {
				c = blend(c, color.NRGBA{0, 0, 0, 255}, 0.10*((v-0.5)/0.5))
			}

			if m := coverage(bars, float64(x), float64(y), fsize); m > 0 {
				c = blend(c, markColor, m)
			}

			c.A = uint8(math.Round(a * 255))
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// renderGlyph draws the menu bar image in c, with the waveform carried by the
// alpha channel.
//
// The idle glyph is drawn black and installed as a template image, which
// means macOS ignores those pixels entirely and recolors the shape for the
// current menu bar appearance. The recording and transcribing glyphs are
// installed as regular images instead, so their color is what shows -- a
// template image cannot be colored at all, which is why there are three
// bitmaps here rather than one plus a tint.
func renderGlyph(size int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bars := glyphBars()
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			a := coverage(bars, float64(x), float64(y), float64(size))
			if a == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(math.Round(a * 255))})
		}
	}
	return img
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(math.Round(float64(a)*(1-t) + float64(b)*t))
}

func write(img image.Image, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// iconsetFiles is the set of sizes iconutil expects, and the name it wants
// each one under. The @2x entries are the same pixel size as the next
// nominal size up, which is why several sizes appear twice.
var iconsetFiles = []struct {
	name string
	size int
}{
	{"icon_16x16.png", 16},
	{"icon_16x16@2x.png", 32},
	{"icon_32x32.png", 32},
	{"icon_32x32@2x.png", 64},
	{"icon_128x128.png", 128},
	{"icon_128x128@2x.png", 256},
	{"icon_256x256.png", 256},
	{"icon_256x256@2x.png", 512},
	{"icon_512x512.png", 512},
	{"icon_512x512@2x.png", 1024},
}

func main() {
	iconset := flag.String("iconset", "", "directory to write the .iconset PNGs into")
	glyph := flag.String("menubar", "", "path to write the 32px menu bar template PNG to")
	glyphDir := flag.String("menubar-states", "", "directory to write the colored menu bar state PNGs into")
	preview := flag.String("preview", "", "path to write a 512px preview PNG to")
	flag.Parse()

	if *iconset != "" {
		// Cache renders by pixel size: half the iconset entries are duplicate
		// sizes under a different name, and the 1024 render is not cheap.
		bySize := map[int]image.Image{}
		for _, f := range iconsetFiles {
			img, ok := bySize[f.size]
			if !ok {
				img = renderIcon(f.size)
				bySize[f.size] = img
			}
			if err := write(img, filepath.Join(*iconset, f.name)); err != nil {
				log.Fatalf("write %s: %v", f.name, err)
			}
		}
		fmt.Printf("wrote %d iconset PNGs to %s\n", len(iconsetFiles), *iconset)
	}

	if *glyph != "" {
		// 32px because systray draws the status item image at 16pt; this is
		// its @2x bitmap, and anything larger is only downscaled at display.
		if err := write(renderGlyph(32, color.NRGBA{0, 0, 0, 255}), *glyph); err != nil {
			log.Fatalf("write %s: %v", *glyph, err)
		}
		fmt.Printf("wrote menu bar glyph to %s\n", *glyph)
	}

	if *glyphDir != "" {
		// Apple's system red and blue, so the states read as status colors
		// rather than arbitrary paint, in both light and dark menu bars.
		states := []struct {
			name string
			c    color.NRGBA
		}{
			{"menubar-recording.png", color.NRGBA{0xFF, 0x3B, 0x30, 0xff}},
			{"menubar-transcribing.png", color.NRGBA{0x0A, 0x84, 0xFF, 0xff}},
		}
		for _, s := range states {
			if err := write(renderGlyph(32, s.c), filepath.Join(*glyphDir, s.name)); err != nil {
				log.Fatalf("write %s: %v", s.name, err)
			}
		}
		fmt.Printf("wrote %d menu bar state glyphs to %s\n", len(states), *glyphDir)
	}

	if *preview != "" {
		if err := write(renderIcon(512), *preview); err != nil {
			log.Fatalf("write %s: %v", *preview, err)
		}
		fmt.Printf("wrote preview to %s\n", *preview)
	}
}
