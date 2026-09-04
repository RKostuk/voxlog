#include <ApplicationServices/ApplicationServices.h>

// See listener_darwin.go for why this trampoline exists. Defined here
// (rather than in the cgo preamble comment) because cgo compiles the
// preamble into more than one translation unit per package when the package
// also exports Go functions (e.g. _cgo_export.c) - a function body in the
// preamble ends up duplicated, and the final link fails with a duplicate
// symbol error. A body in its own .c file is compiled exactly once.
extern CGEventRef goHotkeyEventTapCallback(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *refcon);

CGEventRef hotkeyEventTapTrampoline(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *refcon) {
	return goHotkeyEventTapCallback(proxy, type, event, refcon);
}
