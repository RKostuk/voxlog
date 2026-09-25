package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"voxlog-go/internal/task"

	webview "github.com/webview/webview_go"
)

// Drawer size. Tall enough for a handful of tasks without becoming a second
// main window, narrow enough to sit under the notch without covering the
// menu bar's own items on either side.
const (
	drawerWidth  = 340.0
	drawerHeight = 420.0
)

// DrawerPlacements is every accepted value of
// settings.TasksDrawerPlacement. Deliberately the same vocabulary as the
// recording indicator's placement (see OverlayPositions), minus the two
// cursor modes: a window you type into should not move while you reach for
// it.
var DrawerPlacements = []string{
	OverlayTopCentre, OverlayBottomCentre,
	"top_left", "top_right", "bottom_left", "bottom_right",
}

// DefaultDrawerPlacement is where the drawer opens when the setting is empty
// or unrecognized: top centre, beside the notch, as the user asked for.
const DefaultDrawerPlacement = OverlayTopCentre

// The drawer is a singleton, like the main window: a second one would be two
// views of the same list, both stale the moment the other is used.
var (
	drawerMu        sync.Mutex
	drawerWin       webview.WebView
	drawerPlacement = DefaultDrawerPlacement
	drawerTasks     *task.Store
)

// SetDrawerPlacement records where the drawer opens. Applied on the next
// open rather than immediately: moving a window out from under the user
// while they are typing into it is not a settings change, it is a bug.
func SetDrawerPlacement(placement string) {
	drawerMu.Lock()
	drawerPlacement = normalizeDrawerPlacement(placement)
	drawerMu.Unlock()
}

func normalizeDrawerPlacement(placement string) string {
	for _, p := range DrawerPlacements {
		if p == placement {
			return p
		}
	}
	return DefaultDrawerPlacement
}

// ToggleTasksDrawer opens the drawer, or closes it if it is already up. This
// is what both the hotkey and the tray item call: a drawer you cannot
// dismiss with the same key that opened it is a drawer you end up dragging
// out of the way instead.
func ToggleTasksDrawer(tasks *task.Store) {
	drawerMu.Lock()
	w := drawerWin
	drawerMu.Unlock()

	if w != nil && windowUsable(w) {
		var visible bool
		runOnMainSync(func() { _, _, _, _, visible = describeWindow(w.Window()) })
		if visible {
			HideTasksDrawer()
			return
		}
	}
	ShowTasksDrawer(tasks)
}

// ShowTasksDrawer opens the drawer and gives it keyboard focus, so the add
// box can be typed into straight away.
func ShowTasksDrawer(tasks *task.Store) {
	drawerMu.Lock()
	drawerTasks = tasks
	w := drawerWin
	placement := drawerPlacement
	drawerMu.Unlock()

	if w != nil && windowUsable(w) {
		refreshDrawer()
		runOnMain(func() {
			placeDrawer(w, placement)
			activateApp()
			makeKeyAndOrderFront(w.Window())
		})
		return
	}

	ready := make(chan struct{})
	runOnMain(func() {
		defer close(ready)
		newDrawerWindow(tasks, placement)
	})
	<-ready
}

// HideTasksDrawer orders the drawer off screen without closing it, so the
// next open is instant and the window handle stays valid.
func HideTasksDrawer() {
	drawerMu.Lock()
	w := drawerWin
	drawerMu.Unlock()
	if w == nil || !windowUsable(w) {
		return
	}
	runOnMain(func() { hideWindow(w.Window()) })
}

// placeDrawer puts the window where the placement setting says. Runs on the
// window's dispatch queue, like every other window mutation in this package.
func placeDrawer(w webview.WebView, placement string) {
	resizeWindow(w.Window(), drawerWidth, drawerHeight)
	switch placement {
	case OverlayTopCentre:
		positionCentred(w.Window(), drawerWidth, drawerHeight, true)
	case OverlayBottomCentre:
		positionCentred(w.Window(), drawerWidth, drawerHeight, false)
	default:
		if corner, ok := overlayCorners[placement]; ok {
			positionInCorner(w.Window(), drawerWidth, drawerHeight, corner)
			return
		}
		positionCentred(w.Window(), drawerWidth, drawerHeight, true)
	}
}

// newDrawerWindow builds the window. MUST run on the real OS main thread
// (runOnMain) -- creating an NSWindow anywhere else aborts the process.
func newDrawerWindow(tasks *task.Store, placement string) {
	w := webview.New(false)
	w.SetTitle("Tasks")
	w.SetSize(int(drawerWidth), int(drawerHeight), webview.HintNone)

	w.Bind("drawerClose", func() error {
		HideTasksDrawer()
		return nil
	})

	// One click advances a task to its next status (see NEXT_STATUS in
	// drawer.html). Reuses the store directly rather than the main window's
	// setTaskStatus binding: that one refreshes a window that may not be
	// open, and this one has to refresh the drawer either way.
	w.Bind("drawerSetStatus", func(id, status string) error {
		var next task.Status
		switch status {
		case string(task.StatusTodo), string(task.StatusInProgress), string(task.StatusBlocked), string(task.StatusDone):
			next = task.Status(status)
		default:
			return fmt.Errorf("unknown task status %q", status)
		}
		if err := tasks.Update(id, func(t *task.Task) { t.Status = next }); err != nil {
			return err
		}
		if next == task.StatusDone {
			task.CancelReminder(id)
		}
		refreshDrawer()
		refreshMainFromDrawer()
		return nil
	})

	// A task typed here never went through the classifier, so it has no
	// source: SourceKind and SourceKey stay empty, which is exactly how the
	// rest of the app already reads "written by hand".
	w.Bind("drawerAddTask", func(text, entity string) error {
		if text == "" {
			return nil
		}
		if err := tasks.Append(task.Task{
			ID:      task.NewID(),
			Text:    text,
			Entity:  entity,
			Status:  task.StatusTodo,
			Created: time.Now(),
		}); err != nil {
			return err
		}
		refreshDrawer()
		refreshMainFromDrawer()
		return nil
	})

	w.Bind("drawerOpenMain", func() error {
		HideTasksDrawer()
		if fn := drawerOpenMainHandler(); fn != nil {
			go fn()
		}
		return nil
	})

	html, err := assets.ReadFile("assets/drawer.html")
	if err != nil {
		log.Printf("drawer: %v", err)
		return
	}
	// The same shared kit the main window gets: this page used to carry its
	// own copy of the tokens and helpers, and the two had drifted.
	page := injectAsset(string(html), kitCSSMarker, "kit.css")
	page = injectAsset(page, kitJSMarker, "kit.js")
	w.SetHtml(page)

	drawerMu.Lock()
	drawerWin = w
	drawerMu.Unlock()

	makeDrawerPanel(w.Window())
	keepAliveOnClose(w.Window())
	placeDrawer(w, placement)
	refreshDrawer()
	activateApp()
	makeKeyAndOrderFront(w.Window())
}

// openMainHandler is what the drawer's "Open ↗" button runs. Set by the app
// (SetDrawerOpenMainHandler), for the same reason every other handler in
// this package is: showing the main window needs the stores and the model
// list, which live in package main.
var (
	openMainMu      sync.Mutex
	openMainHandler func()
)

// SetDrawerOpenMainHandler installs what "Open ↗" in the drawer does.
func SetDrawerOpenMainHandler(fn func()) {
	openMainMu.Lock()
	openMainHandler = fn
	openMainMu.Unlock()
}

func drawerOpenMainHandler() func() {
	openMainMu.Lock()
	defer openMainMu.Unlock()
	return openMainHandler
}

// refreshDrawer pushes the current task list into the drawer, if it is open.
// Safe to call from anywhere: it is a no-op without a window.
func refreshDrawer() {
	drawerMu.Lock()
	w := drawerWin
	tasks := drawerTasks
	drawerMu.Unlock()
	if w == nil || tasks == nil || !windowUsable(w) {
		return
	}
	list, err := tasks.All()
	if err != nil {
		log.Printf("drawer: reading tasks: %v", err)
		return
	}
	data, err := json.Marshal(tasksJSON(list))
	if err != nil {
		return
	}
	queue := decodeQueueJSON()
	runOnMain(func() {
		w.Eval(fmt.Sprintf(
			"window.voxlog = window.voxlog || {}; window.voxlog.tasks = %s; window.voxlog.decodeQueue = %s; typeof render === 'function' && render();",
			data, queue))
	})
}

// RefreshTasksDrawerIfOpen is the exported half of refreshDrawer: the app
// calls it after a classification run lands new tasks, so a drawer left open
// on screen shows them without being reopened.
func RefreshTasksDrawerIfOpen() { refreshDrawer() }

// refreshMainFromDrawer keeps the main window in step with an edit made in
// the drawer. It needs the stores the main window was opened with, which
// this package keeps for exactly this kind of cross-window refresh.
func refreshMainFromDrawer() {
	if fn := mainRefresher(); fn != nil {
		fn()
	}
}

var (
	mainRefreshMu sync.Mutex
	mainRefreshFn func()
)

// SetMainRefresher installs how this package refreshes the main window
// without holding the stores itself. Called once at startup.
func SetMainRefresher(fn func()) {
	mainRefreshMu.Lock()
	mainRefreshFn = fn
	mainRefreshMu.Unlock()
}

func mainRefresher() func() {
	mainRefreshMu.Lock()
	defer mainRefreshMu.Unlock()
	return mainRefreshFn
}
