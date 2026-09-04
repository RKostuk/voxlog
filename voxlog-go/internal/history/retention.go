package history

import (
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

// Prune deletes whole day files older than the given policy allows, and
// reports how many it removed. A disabled/unknown policy removes nothing.
//
// Whole files only, never individual entries: day files are named for the
// day they cover, so the cutoff can be decided from the filename without
// parsing (or rewriting) any of them.
func (s *Store) Prune(policy string) (int, error) {
	maxAge, ok := RetentionDuration(policy)
	if !ok {
		return 0, nil
	}

	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0
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
			removed++
		}
	}
	return removed, nil
}
