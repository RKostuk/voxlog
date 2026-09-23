package ui

import (
	"errors"

	"voxlog-go/internal/settings"
)

// The LLM pane's seams. Like the MCP pane's (mcp_bindings.go), these exist so
// this package does not import internal/llm: a window must not be able to
// start a model server, install a Python runtime, or reach into the keychain
// by being opened.

var (
	// llmTestFn checks that the endpoint in the given settings answers, and
	// returns what to show the user.
	llmTestFn func(settings.Settings) error
	// llmKeySetFn stores the API key (empty removes it); llmKeyStoredFn says
	// whether there is one, which is all the pane ever learns about it -- the
	// key itself is never read back into the window.
	llmKeySetFn    func(string) error
	llmKeyStoredFn func() bool
)

// SetLLMTestFunc installs what the Test connection button does.
func SetLLMTestFunc(fn func(settings.Settings) error) {
	winMu.Lock()
	llmTestFn = fn
	winMu.Unlock()
}

// SetLLMKeyFuncs installs how the API key is stored and whether one is.
func SetLLMKeyFuncs(set func(string) error, stored func() bool) {
	winMu.Lock()
	llmKeySetFn, llmKeyStoredFn = set, stored
	winMu.Unlock()
}

func llmTest(v settings.Settings) error {
	winMu.Lock()
	fn := llmTestFn
	winMu.Unlock()
	if fn == nil {
		return errors.New("this build cannot test the connection")
	}
	return fn(v)
}

func llmKeySet(key string) error {
	winMu.Lock()
	fn := llmKeySetFn
	winMu.Unlock()
	if fn == nil {
		return errors.New("this build cannot store an API key")
	}
	return fn(key)
}

func llmKeyStored() bool {
	winMu.Lock()
	fn := llmKeyStoredFn
	winMu.Unlock()
	return fn != nil && fn()
}
