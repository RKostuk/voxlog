package task

import (
	"log"
	"sync"
	"time"

	"voxlog-go/internal/usernotify"
)

// timers holds one AfterFunc per scheduled reminder, keyed by task ID, so a
// status change (or a future edit) can be extended to cancel one later.
// Never persisted -- RescheduleAll rebuilds this from disk on every launch.
var (
	timersMu sync.Mutex
	timers   = map[string]*time.Timer{}
)

// ScheduleReminder arms t's reminder, if it has one and it is still in the
// future. A past reminder is not scheduled here -- RescheduleAll fires those
// once at startup instead, so a reminder that landed while the app was
// closed still reaches the user rather than silently vanishing.
func ScheduleReminder(t Task) {
	if t.Reminder == nil || t.Status == StatusDone {
		return
	}
	d := time.Until(*t.Reminder)
	if d <= 0 {
		return
	}
	timersMu.Lock()
	defer timersMu.Unlock()
	if existing, ok := timers[t.ID]; ok {
		existing.Stop()
	}
	timers[t.ID] = time.AfterFunc(d, func() { fireReminder(t) })
}

// CancelReminder stops a scheduled reminder -- used when a task is marked
// Done before its reminder fires.
func CancelReminder(id string) {
	timersMu.Lock()
	defer timersMu.Unlock()
	if existing, ok := timers[id]; ok {
		existing.Stop()
		delete(timers, id)
	}
}

// RescheduleAll sweeps every non-Done task with a reminder on app startup:
// a reminder that already passed while the app was closed fires once, right
// now, rather than being dropped; a future one is armed normally. Call once,
// after usernotify.Init/SetHandler are wired.
func RescheduleAll(store *Store) {
	all, err := store.All()
	if err != nil {
		log.Printf("task: reschedule: %v", err)
		return
	}
	now := time.Now()
	for _, t := range all {
		if t.Status == StatusDone || t.Reminder == nil {
			continue
		}
		if t.Reminder.Before(now) {
			go fireReminder(t)
			continue
		}
		ScheduleReminder(t)
	}
}

// tasksPane is ui.PaneTasks, spelled out rather than imported: internal/ui
// already imports this package, so taking the constant from there would be an
// import cycle. It is asserted against the real one in reminders_test.go.
const tasksPane = "tasks"

// fireReminder posts the notification. Reuses usernotify's Post(message,
// action) + SetHandler convention already wired up in main.go's notifyPane --
// the action opens the main window on the Tasks pane.
func fireReminder(t Task) {
	usernotify.Post("Reminder: "+t.Text, tasksPane)
}
