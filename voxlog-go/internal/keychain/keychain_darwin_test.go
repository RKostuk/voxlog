//go:build darwin

package keychain

import (
	"errors"
	"strings"
	"testing"
)

// Round-trip through the real login keychain. Skipped rather than failed
// when the process has no keychain access at all -- an unbundled test binary
// on a machine with a locked keychain cannot store anything, and that is the
// environment's answer, not a bug in this package.
func TestSetGetDelete(t *testing.T) {
	const service = "Voxlog test — safe to delete"
	const account = "round-trip"
	t.Cleanup(func() { Delete(service, account) })

	if err := Set(service, account, "sk-secret"); err != nil {
		if strings.Contains(err.Error(), "-34018") || strings.Contains(err.Error(), "-25308") {
			t.Skipf("no keychain access here: %v", err)
		}
		t.Fatal(err)
	}

	got, err := Get(service, account)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Fatalf("got %q, want sk-secret", got)
	}

	// Replacing has to work: a user pasting a new key over an old one is the
	// common case, and add-only would fail on the duplicate.
	if err := Set(service, account, "sk-second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(service, account); got != "sk-second" {
		t.Fatalf("after replacing, got %q, want sk-second", got)
	}

	if err := Delete(service, account); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(service, account); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deleting, Get returned %v, want ErrNotFound", err)
	}
	// Deleting what is already gone is what the caller asked for.
	if err := Delete(service, account); err != nil {
		t.Errorf("deleting a missing item: %v", err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	if _, err := Get("Voxlog test — nothing here", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// Storing an empty secret is the same as not storing one, and must not be
// handed to CFDataCreate as a nil pointer.
func TestSetEmptyRemoves(t *testing.T) {
	const service = "Voxlog test — safe to delete"
	const account = "emptied"
	t.Cleanup(func() { Delete(service, account) })

	if err := Set(service, account, "sk-secret"); err != nil {
		if strings.Contains(err.Error(), "-34018") || strings.Contains(err.Error(), "-25308") {
			t.Skipf("no keychain access here: %v", err)
		}
		t.Fatal(err)
	}
	if err := Set(service, account, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(service, account); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
