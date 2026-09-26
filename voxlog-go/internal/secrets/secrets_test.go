package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSetGetDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s := Open(path)

	if _, err := s.Get(OpenRouterService, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: got %v, want ErrNotFound", err)
	}
	if err := s.Set(OpenRouterService, "a1", "sk-or-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(OpenRouterService, "a2", "sk-or-2"); err != nil {
		t.Fatal(err)
	}
	// A second Store on the same file sees it: nothing lives only in memory.
	again := Open(path)
	if got, _ := again.Get(OpenRouterService, "a2"); got != "sk-or-2" {
		t.Fatalf("got %q, want sk-or-2", got)
	}
	if !again.Exists(OpenRouterService, "a1") || again.Exists(LLMService, LLMAccount) {
		t.Fatal("Exists disagrees with what was stored")
	}

	if err := s.Set(OpenRouterService, "a1", "sk-or-new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(OpenRouterService, "a1"); got != "sk-or-new" {
		t.Fatalf("after replacing, got %q", got)
	}
	if err := s.Delete(OpenRouterService, "a1"); err != nil {
		t.Fatal(err)
	}
	if s.Exists(OpenRouterService, "a1") {
		t.Fatal("deleted key still there")
	}
	if err := s.Delete(OpenRouterService, "nobody"); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
}

// The file holds API keys: only its owner may read it.
func TestFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := Open(path).Set(LLMService, LLMAccount, "sk"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}
