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

// MigrateJSONMeetingsToDB copies every per-meeting JSON record in meetingsDir
// into the database, once. It is the second half of the same story
// MigrateMeetings tells: that one moved meetings out of the day files into
// their own files, this one moves those files into the database, which is the
// only place that can hold a meeting's turns and speakers.
//
// The JSON files are deliberately LEFT ON DISK. They are a frozen backup for
// one release: nothing reads them again after this runs, and if the database
// ever has to be rebuilt, deleting the sentinel re-runs the import.
//
// Safe to re-run by hand for exactly that reason: the insert conflicts on
// start_ns and only fills in a transcript where the row has none, which is
// the same "keep the copy that has text" rule the file store used when two
// files claimed one meeting.
func MigrateJSONMeetingsToDB(meetings *MeetingStore, meetingsDir, sentinelPath string) (int, error) {
	if _, err := os.Stat(sentinelPath); err == nil {
		return 0, nil
	}

	db, err := meetings.open()
	if err != nil {
		return 0, err
	}

	files, err := filepath.Glob(filepath.Join(meetingsDir, "*.json"))
	if err != nil {
		return 0, err
	}

	moved := 0
	for _, f := range files {
		m, err := readMeetingFile(f)
		if err != nil {
			// One unreadable file must not block the import -- and with it
			// the sentinel -- forever, the same call migrateMeetings makes.
			log.Printf("history: migrate: skipping unreadable meeting file %s: %v", f, err)
			continue
		}
		if m.Start.IsZero() {
			log.Printf("history: migrate: skipping meeting file with no start time: %s", f)
			continue
		}

		_, err = db.sql.Exec(`
			INSERT INTO meetings
				(start_ns, recording_secs, decode_secs, text, summary, audio_path, system_audio_path, entity)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(start_ns) DO UPDATE SET
				text    = CASE WHEN meetings.text    = '' THEN excluded.text    ELSE meetings.text    END,
				summary = CASE WHEN meetings.summary = '' THEN excluded.summary ELSE meetings.summary END`,
			m.Start.UnixNano(), m.RecordingSeconds, m.DurationSeconds, m.Text,
			m.Summary, m.AudioPath, m.SystemAudioPath, m.Entity)
		if err != nil {
			return moved, fmt.Errorf("history: migrate: importing %s: %w", f, err)
		}
		moved++
	}

	if err := os.MkdirAll(filepath.Dir(sentinelPath), 0o755); err != nil {
		return moved, fmt.Errorf("history: migrate: creating directory for sentinel %s: %w", sentinelPath, err)
	}
	line := fmt.Sprintf("%s imported %d meeting(s) into the database; the JSON files beside them are a backup and are no longer read\n",
		time.Now().Format(time.RFC3339), moved)
	if err := os.WriteFile(sentinelPath, []byte(line), 0o644); err != nil {
		return moved, fmt.Errorf("history: migrate: writing sentinel %s: %w", sentinelPath, err)
	}

	return moved, nil
}

// MigrateDictations copies every dictation still sitting in a day file into
// the database, once. Same story as MigrateJSONMeetingsToDB, one store over:
// the day files are the release-before-last's storage, and nothing reads them
// after this runs.
//
// The files are deliberately LEFT ON DISK -- a frozen backup for one release.
// Deleting the sentinel re-runs the import, which is safe because Append
// conflicts on ts_ns and re-states rather than duplicating.
//
// Entries marked as meetings are skipped: MigrateMeetings owns those, and it
// runs first for exactly this reason.
func MigrateDictations(days *Store, sentinelPath string) (int, error) {
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
			// One unreadable file must not block the import -- and with it the
			// sentinel -- on every future launch.
			log.Printf("history: migrate: skipping unreadable day file %s: %v", f, err)
			continue
		}
		for _, e := range entries {
			if e.Kind == KindMeeting || e.Timestamp.IsZero() {
				continue
			}
			if err := days.Append(e); err != nil {
				return moved, fmt.Errorf("history: migrate: importing dictation %s from %s: %w",
					e.Timestamp.Format(time.RFC3339Nano), f, err)
			}
			moved++
		}
	}

	if err := writeSentinel(sentinelPath, fmt.Sprintf("imported %d dictation(s)", moved)); err != nil {
		return moved, err
	}
	return moved, nil
}

func writeSentinel(path, what string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("history: migrate: creating directory for sentinel %s: %w", path, err)
	}
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), what)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		return fmt.Errorf("history: migrate: writing sentinel %s: %w", path, err)
	}
	return nil
}
