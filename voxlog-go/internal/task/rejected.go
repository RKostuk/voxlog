package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// maxRejected bounds the negative-example list -- kept small on purpose so
// the prompt it feeds into (see llm.Classify) never grows by more than a
// handful of short lines, no matter how many "Not a task" corrections pile
// up over the app's lifetime. Oldest drops off as new ones come in.
const maxRejected = 8

var rejectedMu sync.Mutex

// rejectedPath lives beside the Tasks directory, not inside it -- Store.All
// globs every *.json file in s.dir and would otherwise try to parse this as
// a Task.
func (s *Store) rejectedPath() string {
	return filepath.Join(filepath.Dir(s.dir), "task-rejected.json")
}

// LoadRejected returns the extracted task text of everything the user has
// explicitly marked "Not a task" (as opposed to a plain Delete, which
// records nothing -- see main_window.go's setTaskStatus/deleteTask/
// rejectTask bindings). Missing file is not an error, just no history yet.
func (s *Store) LoadRejected() ([]string, error) {
	raw, err := os.ReadFile(s.rejectedPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// AppendRejected records one more misclassification and trims the list back
// to maxRejected, oldest first out.
func (s *Store) AppendRejected(text string) error {
	rejectedMu.Lock()
	defer rejectedMu.Unlock()

	list, err := s.LoadRejected()
	if err != nil {
		list = nil
	}
	list = append(list, text)
	if len(list) > maxRejected {
		list = list[len(list)-maxRejected:]
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	path := s.rejectedPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
