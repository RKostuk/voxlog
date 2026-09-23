package mcp

import (
	"os"
	"testing"
)

// useTempTokenDir keeps a test out of the real Application Support directory:
// regenerating there would lock the user's own clients out of their server.
func useTempTokenDir(t *testing.T) {
	t.Helper()
	previous := tokenDir
	tokenDir = t.TempDir()
	t.Cleanup(func() { tokenDir = previous })
}

func TestTheTokenFileIsReadableOnlyByItsOwner(t *testing.T) {
	useTempTokenDir(t)

	if _, err := LoadOrCreateToken(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the token file is %v, want 0600", perm)
	}
}

// An MCP client is configured once and has to find the same token after a
// restart -- this is the whole reason it is stored rather than generated.
func TestTheTokenSurvivesASecondLoad(t *testing.T) {
	useTempTokenDir(t)

	first, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the token changed between loads: %s then %s", first, second)
	}
}

func TestRegenerateReplacesTheToken(t *testing.T) {
	useTempTokenDir(t)

	first, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := RegenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("regenerating produced the same token")
	}
	stored, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if stored != second {
		t.Fatalf("the new token was not the one stored: %s", stored)
	}
}

// A token file somebody opened in an editor and saved still works.
func TestATrailingNewlineIsIgnored(t *testing.T) {
	useTempTokenDir(t)

	if err := os.WriteFile(TokenPath(), []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "abc123" {
		t.Fatalf("got %q, want abc123", token)
	}
}
