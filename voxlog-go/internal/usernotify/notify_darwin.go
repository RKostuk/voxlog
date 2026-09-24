// Package usernotify posts the banners Voxlog shows when something finishes,
// and routes a click on one back into the app.
package usernotify

/*
// -fobjc-arc for the same reason systemaudio needs it: notify_darwin.m is
// written in ARC style, and cgo compiles .m files without ARC by default.
#cgo CFLAGS: -x objective-c -fobjc-arc -fmodules
#cgo LDFLAGS: -framework Foundation -framework UserNotifications
#include <stdlib.h>

void voxlogNotifyInit(void);
int voxlogNotifyPost(const char *message, const char *action);
*/
import "C"

import (
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"
)

var (
	mu      sync.Mutex
	handler func(action string)

	// decided flips to true once the permission completion handler has run
	// (granted or not) -- see goNotifyAuthDecided. pending holds messages
	// that arrived before that, so they get the CORRECT delivery path
	// (native, with Voxlog's own icon, if granted) instead of always taking
	// the osascript fallback purely because the answer wasn't in yet.
	decided     bool
	pending     []pendingPost
	pendingWait *time.Timer

	// granted is the answer itself, and fellBack records that a post had to
	// go out through osascript. Together they are the only evidence the user
	// can be shown for "the banner you expected never appeared": macOS
	// refusing the permission is silent everywhere else, and a courtesy
	// notice nobody sees is worse than no feature at all.
	granted  bool
	fellBack bool
)

// State is what the settings window shows about banners: whether macOS has
// answered the permission request at all, what it answered, and whether
// anything has had to fall back to osascript since.
type State struct {
	Decided  bool
	Granted  bool
	FellBack bool
}

func Status() State {
	mu.Lock()
	defer mu.Unlock()
	return State{Decided: decided, Granted: granted, FellBack: fellBack}
}

type pendingPost struct{ message, action string }

// warnFallbackOnce keeps the osascript-fallback warning to a single line per
// run. It is the one symptom that explains both "wrong icon" and "clicking
// the banner opens Finder", and without it the fallback is silent -- which is
// exactly why those two were diagnosed as separate bugs.
var warnFallbackOnce sync.Once

// pendingTimeout is the safety net for a decision that never arrives (an
// unbundled dev build, per voxlogNotifyInit's early return -- gDecided is
// never set in that case). Past this, queued messages are flushed via the
// osascript fallback rather than held forever.
const pendingTimeout = 3 * time.Second

// Init asks for permission to post notifications. Call it once, early --
// before anything that could call Post, ideally before whatever might
// trigger the very first one (e.g. a hotkey listener) starts accepting
// input, so Post has the best chance of queuing rather than immediately
// falling back to osascript banners.
func Init() { C.voxlogNotifyInit() }

// SetHandler registers what a click on a banner does. action is the string
// that was passed to Post.
func SetHandler(fn func(action string)) {
	mu.Lock()
	handler = fn
	mu.Unlock()
}

//export goNotificationAction
func goNotificationAction(action *C.char) {
	mu.Lock()
	fn := handler
	mu.Unlock()
	if fn == nil {
		return
	}
	// Copy before returning: the C string is the NSString's buffer, valid
	// only for the length of the delegate callback.
	go fn(C.GoString(action))
}

// goNotifyAuthDecided runs once, from the permission-request completion
// handler (notify_darwin.m), whether granted or refused. Draining here
// rather than in Post means a message queued during the undecided window
// is replayed through the same postNow path Post itself uses, so it gets
// exactly the delivery Post would have given it had the answer already
// been in.
//
//export goNotifyAuthDecided
func goNotifyAuthDecided(allowed C.int) {
	mu.Lock()
	decided = true
	granted = allowed == 1
	if pendingWait != nil {
		pendingWait.Stop()
		pendingWait = nil
	}
	queued := pending
	pending = nil
	mu.Unlock()

	for _, p := range queued {
		postNow(p.message, p.action)
	}
}

// Post shows a banner. A non-empty action is handed to the SetHandler
// callback if the user clicks it.
//
// While authorization is still undecided, the message is queued (see
// goNotifyAuthDecided) rather than posted immediately -- posting now would
// mean always taking the osascript fallback, which can never show Voxlog's
// own icon (AppleScript's `display notification` has no icon parameter) and
// needs its own, separate, unrequested Notification Center permission that
// silently no-ops when refused.
func Post(message, action string) {
	mu.Lock()
	if !decided {
		pending = append(pending, pendingPost{message, action})
		if pendingWait == nil {
			pendingWait = time.AfterFunc(pendingTimeout, flushPendingTimedOut)
		}
		mu.Unlock()
		return
	}
	mu.Unlock()
	postNow(message, action)
}

// flushPendingTimedOut runs if goNotifyAuthDecided never fires within
// pendingTimeout -- an unbundled dev build (voxlogNotifyInit returns before
// ever requesting authorization) is the one case that's actually expected;
// anything else would be a bug in the ObjC completion handler. Either way,
// a held-forever notification is worse than a wrong-icon one.
func flushPendingTimedOut() {
	mu.Lock()
	queued := pending
	pending = nil
	pendingWait = nil
	mu.Unlock()

	for _, p := range queued {
		postNow(p.message, p.action)
	}
}

func postNow(message, action string) {
	cMessage, cAction := C.CString(message), C.CString(action)
	defer C.free(unsafe.Pointer(cMessage))
	defer C.free(unsafe.Pointer(cAction))

	if C.voxlogNotifyPost(cMessage, cAction) == 1 {
		return
	}
	mu.Lock()
	fellBack = true
	mu.Unlock()
	warnFallbackOnce.Do(func() {
		log.Print("notify: falling back to osascript banners -- the native " +
			"path was refused or unavailable, so banners will carry " +
			"osascript's icon and a click on one will activate osascript " +
			"(Finder) instead of Voxlog. Run the app bundle (make bundle), " +
			"then check System Settings > Notifications for Voxlog")
	})
	postViaOSAScript(message)
}

// postViaOSAScript is the pre-UserNotifications path, kept as the fallback
// for a refused permission. The message is escaped: it carries error
// strings, and an unescaped quote turns the whole thing into a syntax error
// rather than a notification.
func postViaOSAScript(message string) {
	msg := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(message)
	if err := exec.Command("osascript", "-e",
		`display notification "`+msg+`" with title "Voxlog"`).Run(); err != nil {
		log.Printf("notify: %v", err)
	}
}
