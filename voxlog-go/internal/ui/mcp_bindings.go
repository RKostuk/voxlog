package ui

import (
	"errors"

	"github.com/webview/webview_go"

	"voxlog-go/internal/output"
	"voxlog-go/internal/settings"
)

// The MCP pane's seams. This package deliberately does not import
// internal/mcp -- for the same reason it does not reach into the decode
// queue: the server's lifetime belongs to the app, and a window should not
// be able to start one by being opened.

var (
	// settingsAppliedFn is told about every saved settings change. Nothing
	// else in the app learns of one today (always-on only gets away with it
	// by polling the store every two seconds), and a switch that starts and
	// stops a listener cannot poll.
	settingsAppliedFn func(settings.Settings)
	// mcpStatusFn reports what the MCP pane shows; mcpRegenerateFn issues a
	// new token and returns it.
	mcpStatusFn     func() map[string]any
	mcpRegenerateFn func() (string, error)
)

// SetSettingsAppliedFunc installs what runs after settings are saved.
func SetSettingsAppliedFunc(fn func(settings.Settings)) {
	winMu.Lock()
	settingsAppliedFn = fn
	winMu.Unlock()
}

// SetMCPStatusFunc installs the MCP pane's status source.
func SetMCPStatusFunc(fn func() map[string]any) {
	winMu.Lock()
	mcpStatusFn = fn
	winMu.Unlock()
}

// SetMCPRegenerateFunc installs what the Regenerate token button does.
func SetMCPRegenerateFunc(fn func() (string, error)) {
	winMu.Lock()
	mcpRegenerateFn = fn
	winMu.Unlock()
}

// settingsApplied runs the installed callback, if there is one. A window
// opened before startup finished has none, and that is not a fault.
func settingsApplied(v settings.Settings) {
	winMu.Lock()
	fn := settingsAppliedFn
	winMu.Unlock()
	if fn != nil {
		fn(v)
	}
}

// bindMCP registers the MCP pane's two bindings.
func bindMCP(w webview.WebView) {
	w.Bind("mcpStatus", func() (map[string]any, error) {
		winMu.Lock()
		fn := mcpStatusFn
		winMu.Unlock()
		if fn == nil {
			return map[string]any{"enabled": false, "running": false}, nil
		}
		return fn(), nil
	})

	// copyText puts a string on the clipboard. The MCP pane's connect
	// snippets carry the token, and building them in Go rather than in the
	// page keeps the one credential here out of the DOM's reach until the
	// user actually asks for it.
	w.Bind("copyText", func(text string) error {
		output.WriteClipboard(text)
		return nil
	})

	w.Bind("mcpRegenerateToken", func() (string, error) {
		winMu.Lock()
		fn := mcpRegenerateFn
		winMu.Unlock()
		if fn == nil {
			return "", errors.New("the MCP server is not available")
		}
		return fn()
	})
}
