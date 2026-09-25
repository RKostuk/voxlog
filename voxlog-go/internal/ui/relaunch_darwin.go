package ui

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <dlfcn.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

// Security.framework's translocation calls are exported but not in the
// public headers, so they are looked up at run time: a macOS that drops
// them leaves the path as it was instead of failing to launch at all.
typedef CFURLRef (*originalPathFn)(CFURLRef, CFErrorRef *);

// voxlogOriginalPath writes the path an app translocated by Gatekeeper was
// really opened from into out, and returns 1; 0 when there is none.
static int voxlogOriginalPath(const char *path, char *out, long outLen) {
	void *h = dlopen("/System/Library/Frameworks/Security.framework/Security", RTLD_LAZY);
	if (!h) return 0;
	originalPathFn fn = (originalPathFn)dlsym(h, "SecTranslocateCreateOriginalPathForURL");
	if (!fn) return 0;
	CFURLRef in = CFURLCreateFromFileSystemRepresentation(NULL, (const UInt8 *)path, strlen(path), true);
	if (!in) return 0;
	CFURLRef orig = fn(in, NULL);
	CFRelease(in);
	if (!orig) return 0;
	Boolean ok = CFURLGetFileSystemRepresentation(orig, true, (UInt8 *)out, outLen);
	CFRelease(orig);
	return ok ? 1 : 0;
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// relaunchTarget is what a relaunch should open: the .app this binary sits
// in, or the bare binary when run unbundled. An app run straight out of
// Downloads is translocated by Gatekeeper to a read-only mount that is torn
// down as this process exits, so opening that path afterwards finds nothing
// -- the relaunch goes to where the app really lives instead.
func relaunchTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// From <bundle>/Contents/MacOS/voxlog-go, walk back up to the .app so the
	// relaunch keeps the bundle identity the permissions are attached to.
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if !strings.HasSuffix(app, ".app") {
		return exe, nil
	}
	if strings.Contains(app, "/AppTranslocation/") {
		buf := make([]byte, 4096)
		cpath := C.CString(app)
		defer C.free(unsafe.Pointer(cpath))
		if C.voxlogOriginalPath(cpath, (*C.char)(unsafe.Pointer(&buf[0])), C.long(len(buf))) == 1 {
			if n := strings.IndexByte(string(buf), 0); n > 0 {
				return string(buf[:n]), nil
			}
		}
	}
	return app, nil
}

// startRelaunch hands the relaunch to a shell that outlives this process:
// it waits until this PID is gone, then opens the app again with args. The
// replacement never overlaps this instance, and nothing depends on how long
// a fixed sleep happened to be.
func startRelaunch(target string, args ...string) error {
	var open string
	if strings.HasSuffix(target, ".app") {
		open = `/usr/bin/open -n "$1" --args "${@:2}"`
	} else {
		open = `"$1" "${@:2}" >/dev/null 2>&1 &`
	}
	script := fmt.Sprintf(`while /bin/kill -0 %d 2>/dev/null; do /bin/sleep 0.1; done; %s`, os.Getpid(), open)
	cmd := exec.Command("/bin/bash", append([]string{"-c", script, "voxlog-relaunch", target}, args...)...)
	// Its own session, so nothing that happens to this process on the way
	// out reaches the helper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
