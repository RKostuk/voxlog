package history

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// MigrateMeetings moves every meeting entry out of the day files into the
// meeting store, once. It exists because meetings used to live inside the
// day files (Task 1 gave them their own store, but did nothing about the
// meetings already sitting in old files) and there is no other point in the
// program that will ever revisit those files again.
//
// It is safe to run on every launch, or to delete the sentinel and re-run by
// hand: a day file is only rewritten after its meetings have already landed
// in the meeting store, so at worst a crash between the two leaves a
// meeting written twice, under the same filename, and MeetingStore.All
// already collapses that back down to one record. The sentinel exists only
// to skip the day-file scan once it has nothing left to find.
func MigrateMeetings(days *Store, meetings *MeetingStore, sentinelPath string) (int, error) {
	if _, err := os.Stat(sentinelPath); err == nil {
		return 0, nil
	}

	files, err := filepath.Glob(filepath.Join(days.Dir(), "*.json"))
	if err != nil {
		return 0, err
	}

	moved := 0
	for _, f := range files {
		entries, err := days.readDay(f)
		if err != nil {
			// A file nothing else can parse has nothing recoverable to move;
			// skip it so one bad file doesn't block migration (and the
			// sentinel) forever, as it would on every future launch.
			log.Printf("history: migrate: skipping unreadable file %s: %v", f, err)
			continue
		}

		hasMeeting := false
		for _, e := range entries {
			if e.Kind == KindMeeting {
				hasMeeting = true
				break
			}
		}
		if !hasMeeting {
			continue // untouched file, nothing to lose by leaving it that way
		}

		kept := entries[:0:0]
		for _, e := range entries {
			if e.Kind != KindMeeting {
				kept = append(kept, e)
				continue
			}
			// Written before the day file is rewritten below: a crash here
			// leaves a duplicate the store collapses, not a dropped meeting.
			if err := meetings.Append(Meeting{
				Start:            e.Timestamp,
				RecordingSeconds: e.RecordingSeconds,
				Text:             e.Text,
				DurationSeconds:  e.DurationSeconds,
				AudioPath:        e.AudioPath,
				SystemAudioPath:  e.SystemAudioPath,
			}); err != nil {
				return moved, fmt.Errorf("history: migrate: moving meeting %s from %s: %w", e.Timestamp.Format(time.RFC3339Nano), f, err)
			}
			moved++
		}

		if err := days.writeDay(f, kept); err != nil {
			return moved, fmt.Errorf("history: migrate: rewriting day file %s: %w", f, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(sentinelPath), 0o755); err != nil {
		return moved, fmt.Errorf("history: migrate: creating directory for sentinel %s: %w", sentinelPath, err)
	}
	line := fmt.Sprintf("%s moved %d meeting(s)\n", time.Now().Format(time.RFC3339), moved)
	if err := os.WriteFile(sentinelPath, []byte(line), 0o644); err != nil {
		return moved, fmt.Errorf("history: migrate: writing sentinel %s: %w", sentinelPath, err)
	}

	return moved, nil
}
