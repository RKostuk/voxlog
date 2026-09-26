// Package secrets keeps the LLM providers' API keys in their own file,
// secrets.json beside settings.json, readable only by the user (0600).
//
// Not the login keychain, which is where they first lived: a keychain item
// remembers which build of the app may read it, and every build of a
// self-signed app is a new one -- so each rebuild, and each update, met the
// user with a password prompt on the first Test connection or task. Not
// settings.json either: that file gets opened, pasted into bug reports and
// synced, and a key does not belong in any of those.
package secrets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound is returned by Get when nothing has been stored yet -- the
// normal state of a provider nobody has configured.
var ErrNotFound = errors.New("secrets: no such key")

// Where each provider's keys are filed. The generic API has exactly one
// key; OpenRouter has one per account, filed under the account's ID
// (settings.OpenRouterAccount.ID).
const (
	LLMService        = "llm-api"
	LLMAccount        = "api-key"
	OpenRouterService = "openrouter"
)

// Store is the secrets file. Every call reads or rewrites the whole file:
// it holds a handful of short strings, and there is then no in-memory copy
// that could disagree with the disk.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open returns the store at path. The file need not exist yet.
func Open(path string) *Store { return &Store{path: path} }

// DefaultPath is secrets.json in the same directory as settingsPath.
func DefaultPath(settingsPath string) string {
	return filepath.Join(filepath.Dir(settingsPath), "secrets.json")
}

type fileShape map[string]map[string]string

func (s *Store) load() (fileShape, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileShape{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := fileShape{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// save writes to a temporary file created 0600 and renames it into place,
// so the key is never on disk with wider permissions and a crash mid-write
// leaves the old file whole.
func (s *Store) save(data fileShape) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Set stores secret under service/account, replacing anything there. An
// empty secret removes it.
func (s *Store) Set(service, account, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return err
	}
	if secret == "" {
		if _, ok := data[service][account]; !ok {
			return nil
		}
		delete(data[service], account)
		if len(data[service]) == 0 {
			delete(data, service)
		}
		return s.save(data)
	}
	if data[service] == nil {
		data[service] = map[string]string{}
	}
	data[service][account] = secret
	return s.save(data)
}

// Get returns the stored secret, or ErrNotFound.
func (s *Store) Get(service, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return "", err
	}
	secret, ok := data[service][account]
	if !ok {
		return "", ErrNotFound
	}
	return secret, nil
}

// Exists reports whether a secret is stored.
func (s *Store) Exists(service, account string) bool {
	_, err := s.Get(service, account)
	return err == nil
}

// Delete removes the secret; removing one that is not there is not an error.
func (s *Store) Delete(service, account string) error { return s.Set(service, account, "") }
