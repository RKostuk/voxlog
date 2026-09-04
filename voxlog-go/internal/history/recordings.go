package history

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// isRecording reports whether entry is one this sweep is allowed to touch:
// a top-level .wav file. Anything else -- subdirectories, other file types --
// belongs to someone else and must be left alone.
func isRecording(entry os.DirEntry) bool {
	return !entry.IsDir() && filepath.Ext(entry.Name()) == ".wav"
}

// RecordingsSize reports what the recordings directory currently occupies.
func RecordingsSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil // not created yet, so it holds nothing
	}
	if err != nil {
		return 0, err
	}

	var total int64
	for _, entry := range entries {
		if !isRecording(entry) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

type recording struct {
	path    string
	size    int64
	modTime time.Time
}

// SweepRecordings deletes recordings the settings no longer want kept:
// anything older than the age policy allows, then oldest-first until the
// folder fits under maxBytes. inUse names files that must never be deleted
// whatever the policy says -- a meeting still being recorded is holding its
// WAV open and still growing it.
//
// A disabled policy (RetentionDisabled, or maxBytes <= 0) does nothing.
func SweepRecordings(dir, policy string, maxBytes int64, inUse map[string]bool) (removed int, freed int64, err error) {
	maxAge, ageOn := RetentionDuration(policy)
	ceilingOn := maxBytes > 0
	if !ageOn && !ceilingOn {
		return 0, 0, nil
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	var candidates []recording
	var total int64
	for _, entry := range entries {
		if !isRecording(entry) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if inUse[path] {
			continue // still being written; never a candidate
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		candidates = append(candidates, recording{path: path, size: info.Size(), modTime: info.ModTime()})
	}

	remove := func(r recording) {
		if err := os.Remove(r.path); err != nil {
			// One locked file must not stop the sweep; its bytes are still
			// on disk, so they still count toward the total.
			log.Printf("history: could not remove recording %s: %v", r.path, err)
			return
		}
		removed++
		freed += r.size
		total -= r.size
	}

	if ageOn {
		cutoff := time.Now().Add(-maxAge)
		var kept []recording
		for _, r := range candidates {
			if r.modTime.Before(cutoff) {
				remove(r)
				continue
			}
			kept = append(kept, r)
		}
		candidates = kept
	}

	if ceilingOn && total > maxBytes {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].modTime.Before(candidates[j].modTime)
		})
		for _, r := range candidates {
			if total <= maxBytes {
				break
			}
			remove(r)
		}
	}

	return removed, freed, nil
}
