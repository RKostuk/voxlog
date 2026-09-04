// Package sfsymbol renders SF Symbols to PNG bytes.
//
// Its own package rather than a file in ui: cgo merges every CFLAGS line in a
// package, and this needs -fobjc-arc, which ui's hand-written objc_msgSend
// preamble does not compile under.
package sfsymbol

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fmodules
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>

void *voxlogSymbolPNG(const char *name, double points, int r, int g, int b,
                      int tinted, int *outLen);
*/
import "C"

import (
	"sync"
	"unsafe"
)

// PNG renders an SF Symbol as a PNG for a menu item's template icon:
// black glyph, alpha shape, recoloured by macOS to match the menu.
//
// Returns nil if this macOS has no such symbol -- callers show the item
// without an icon rather than with a hole in it.
func PNG(name string, points float64) []byte {
	return symbol(name, points, 0, 0, 0, false)
}

// TintedPNG is PNG in a fixed colour, for the status item's own image:
// the menu bar's recording states are regular (non-template) images precisely
// so their colour survives (see MenuBarIconRecording).
func TintedPNG(name string, points float64, r, g, b uint8) []byte {
	return symbol(name, points, r, g, b, true)
}

// symbols are asked for on every menu state change; the bitmaps never change,
// so render each one once.
var (
	symbolMu    sync.Mutex
	symbolCache = map[string][]byte{}
)

func symbol(name string, points float64, r, g, b uint8, tinted bool) []byte {
	key := name + string(rune(int(points))) + string([]byte{r, g, b, boolByte(tinted)})
	symbolMu.Lock()
	if png, ok := symbolCache[key]; ok {
		symbolMu.Unlock()
		return png
	}
	symbolMu.Unlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var n C.int
	tint := C.int(0)
	if tinted {
		tint = 1
	}
	buf := C.voxlogSymbolPNG(cName, C.double(points), C.int(r), C.int(g), C.int(b), tint, &n)
	if buf == nil || n == 0 {
		return nil
	}
	png := C.GoBytes(buf, n)
	C.free(buf)

	symbolMu.Lock()
	symbolCache[key] = png
	symbolMu.Unlock()
	return png
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
