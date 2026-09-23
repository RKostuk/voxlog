package history

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Retention policies, as stored in Settings.HistoryRetention.
const (
	RetentionDisabled = "disabled"
	RetentionWeek     = "week"
	RetentionTwoWeeks = "two_weeks"
	RetentionMonth    = "month"
)

// RetentionDuration maps a policy name to how long entries are kept.
// RetentionDisabled (and any unrecognized value) returns ok=false, meaning
// "keep everything" -- an unknown policy must never be read as "delete
// aggressively".
func RetentionDuration(policy string) (time.Duration, bool) {
	switch policy {
	case RetentionWeek:
		return 7 * 24 * time.Hour, true
	case RetentionTwoWeeks:
		return 14 * 24 * time.Hour, true
	case RetentionMonth:
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// Prune deletes dictations older than the given policy allows and reports how
// many went. A disabled/unknown policy removes nothing.
//
// One row at a time now that dictations are rows. This used to delete whole
// day files, because a day was the unit on disk and deleting half of one would
// have meant rewriting it -- which also meant a transcript was kept up to a
// day longer than asked for.
//
// Old day files still on disk are swept by the same call, by filename as
// before: they are last release's copy of data that now lives in the database,
// and the retention setting covers them too.
func (s *Store) Prune(policy string) (int, error) {
	maxAge, ok := RetentionDuration(policy)
	if !ok {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge)

	db, err := s.open()
	if err != nil {
		return 0, err
	}
	res, err := db.sql.Exec("DELETE FROM dictations WHERE ts_ns < ?", cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("history: pruning dictations: %w", err)
	}
	n, _ := res.RowsAffected()
	removed := int(n)

	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return removed, err
	}
	for _, f := range files {
		name := filepath.Base(f)
		day, err := time.ParseInLocation("2006-01-02", name[:len(name)-len(".json")], time.Local)
		if err != nil {
			continue // not a day file (stray .json); leave it alone
		}
		// Compare against the END of that day, so a file is only dropped
		// once every entry it could hold is past the cutoff.
		if day.AddDate(0, 0, 1).Before(cutoff) {
			if err := os.Remove(f); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}
