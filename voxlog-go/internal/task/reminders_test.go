package task

import "testing"

// The pane a reminder's banner opens on. Written out here rather than taken
// from internal/ui, which imports this package -- the point of the test is
// that the hand-written copy in reminders.go and the window's own constant
// cannot drift apart silently. ui.PaneTasks is asserted against the same
// literal in internal/ui's own tests.
func TestReminderOpensTheTasksPane(t *testing.T) {
	if tasksPane != "tasks" {
		t.Errorf("tasksPane = %q, want \"tasks\" (ui.PaneTasks)", tasksPane)
	}
}
