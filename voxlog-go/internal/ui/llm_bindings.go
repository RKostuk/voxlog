package ui

import (
	"errors"

	"voxlog-go/internal/llm"
	"voxlog-go/internal/settings"
)

// The LLM pane's seams. Like the MCP pane's (mcp_bindings.go), these exist so
// this package does not import internal/llm: a window must not be able to
// start a model server, install a Python runtime, or read an API key
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

	// The OpenRouter accounts' keys, one per account ID, under the same
	// write-only rule: the pane stores and removes them, and learns only
	// which accounts have one.
	orKeySetFn     func(id, key string) error
	orKeysStoredFn func(ids []string) map[string]bool
	// orFreeModelsFn fetches OpenRouter's current free models.
	orFreeModelsFn func() ([]llm.ModelInfo, error)
	// orLimitsFn says, per account ID, how today's free requests stand.
	orLimitsFn func(ids []string) map[string]OpenRouterQuota
)

// OpenRouterQuota is one account's free requests today, as the window sees
// it. Limit is 0 when the count could not be read; Reset (Unix
// milliseconds) is set only once they are used up.
type OpenRouterQuota struct {
	Used      int   `json:"used"`
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// SetOpenRouterLimitsFunc installs what the panes ask to show each
// account's free requests left today.
func SetOpenRouterLimitsFunc(fn func(ids []string) map[string]OpenRouterQuota) {
	winMu.Lock()
	orLimitsFn = fn
	winMu.Unlock()
}

func orLimits(ids []string) map[string]OpenRouterQuota {
	winMu.Lock()
	fn := orLimitsFn
	winMu.Unlock()
	if fn == nil {
		return map[string]OpenRouterQuota{}
	}
	return fn(ids)
}

// SetOpenRouterFuncs installs the OpenRouter accounts' key storage and the
// free-model list behind the pane's Refresh button.
func SetOpenRouterFuncs(setKey func(id, key string) error, stored func(ids []string) map[string]bool, freeModels func() ([]llm.ModelInfo, error)) {
	winMu.Lock()
	orKeySetFn, orKeysStoredFn, orFreeModelsFn = setKey, stored, freeModels
	winMu.Unlock()
}

func orKeySet(id, key string) error {
	winMu.Lock()
	fn := orKeySetFn
	winMu.Unlock()
	if fn == nil {
		return errors.New("this build cannot store an OpenRouter key")
	}
	if id == "" {
		return errors.New("no account to store the key under")
	}
	return fn(id, key)
}

func orKeysStored(ids []string) map[string]bool {
	winMu.Lock()
	fn := orKeysStoredFn
	winMu.Unlock()
	if fn == nil {
		return map[string]bool{}
	}
	return fn(ids)
}

func orFreeModels() ([]llm.ModelInfo, error) {
	winMu.Lock()
	fn := orFreeModelsFn
	winMu.Unlock()
	if fn == nil {
		return nil, errors.New("this build cannot list OpenRouter models")
	}
	return fn()
}

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
