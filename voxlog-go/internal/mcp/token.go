package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"voxlog-go/internal/settings"
)

// tokenFileName is the file the bearer token lives in, beside settings.json.
//
// Deliberately NOT inside settings.json: that file is written 0644 (see
// settings.Store.Save), it is the one users hand-edit, diff and screenshot,
// and every one of its values travels through the webview on the way to the
// settings pane. A credential belongs in a file of its own, readable only by
// the user who owns it.
const tokenFileName = "mcp-token"

// tokenDir overrides where the token lives. Empty means beside settings.json,
// which is the only value the app ever uses; the tests set it so that running
// them cannot overwrite the token of the Voxlog installed on this machine.
var tokenDir string

// UseTokenDir points the token at another directory. Only the tests call it,
// and only so that running them cannot overwrite the token of the Voxlog
// installed on this machine; "" puts it back beside settings.json.
func UseTokenDir(dir string) { tokenDir = dir }

// TokenPath is where the token is kept.
func TokenPath() string {
	dir := tokenDir
	if dir == "" {
		dir = filepath.Dir(settings.Path())
	}
	return filepath.Join(dir, tokenFileName)
}

// LoadOrCreateToken returns the stored token, generating and saving one the
// first time.
//
// Stored rather than generated per run, which is where this differs from the
// window's page server: that server's only client is a webview created
// milliseconds later, so a fresh token each launch costs nothing. An MCP
// client holds its configuration across reboots, and a token that changed on
// every launch would break it on every launch.
func LoadOrCreateToken() (string, error) {
	raw, err := os.ReadFile(TokenPath())
	if err == nil {
		if token := string(trimSpace(raw)); token != "" {
			return token, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("mcp: reading the token: %w", err)
	}
	return RegenerateToken()
}

// RegenerateToken writes a brand new token, invalidating every client that
// holds the old one.
func RegenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mcp: generating a token: %w", err)
	}
	token := hex.EncodeToString(buf)

	path := TokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mcp: %w", err)
	}
	// 0600, and written through a temporary file so a crash mid-write cannot
	// leave a truncated token behind that locks the user out of their own
	// server with no way to tell why.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("mcp: writing the token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("mcp: writing the token: %w", err)
	}
	return token, nil
}

// trimSpace drops trailing whitespace, so a token file somebody opened in an
// editor and saved still works.
func trimSpace(b []byte) []byte {
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != '\n' && c != '\r' && c != ' ' && c != '\t' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
