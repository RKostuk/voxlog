package main

import (
	"testing"

	"voxlog-go/internal/keychain"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/settings"
)

func fakeKeys(items map[string]string) func(service, account string) (string, error) {
	return func(service, account string) (string, error) {
		if k, ok := items[service+"/"+account]; ok {
			return k, nil
		}
		return "", keychain.ErrNotFound
	}
}

func TestOpenRouterEndpointUsesTheActiveAccount(t *testing.T) {
	cfg := settings.DefaultSettings()
	cfg.LLMProvider = settings.LLMProviderOpenRouter
	cfg.OpenRouterAccounts = []settings.OpenRouterAccount{{ID: "a1", Label: "Main"}, {ID: "a2", Label: "Spare"}}
	cfg.OpenRouterActive = "a2"
	cfg.OpenRouterModel = "m/one:free"
	cfg.OpenRouterFallbacks = []string{"m/two:free", "m/three:free", "m/four:free"}
	keys := fakeKeys(map[string]string{
		keychain.OpenRouterService + "/a1": "sk-or-main",
		keychain.OpenRouterService + "/a2": "sk-or-spare",
	})

	ep := endpointFor(cfg, keys)
	if ep.BaseURL != llm.OpenRouterBaseURL || ep.Model != "m/one:free" {
		t.Errorf("endpoint = %+v", ep)
	}
	if ep.APIKey != "sk-or-spare" {
		t.Errorf("key = %q, want the active account's", ep.APIKey)
	}
	if len(ep.Fallbacks) != settings.MaxOpenRouterFallbacks {
		t.Errorf("fallbacks = %v, want capped at %d", ep.Fallbacks, settings.MaxOpenRouterFallbacks)
	}
}

// With no model picked there is nothing to ask, and a remote-looking
// endpoint would make Task Hub think there were.
func TestOpenRouterWithoutAModelIsNotReady(t *testing.T) {
	cfg := settings.DefaultSettings()
	cfg.LLMProvider = settings.LLMProviderOpenRouter
	if ep := endpointFor(cfg, fakeKeys(nil)); ep.Remote() {
		t.Errorf("endpoint = %+v, want the zero endpoint", ep)
	}
}

func TestAPIEndpointIsUnchanged(t *testing.T) {
	cfg := settings.DefaultSettings()
	cfg.LLMProvider = settings.LLMProviderAPI
	cfg.LLMBaseURL = "https://api.openai.com"
	cfg.LLMModel = "gpt-4o-mini"
	ep := endpointFor(cfg, fakeKeys(map[string]string{keychain.LLMService + "/" + keychain.LLMAccount: "sk"}))
	if ep.BaseURL != cfg.LLMBaseURL || ep.Model != "gpt-4o-mini" || ep.APIKey != "sk" || ep.Fallbacks != nil {
		t.Errorf("endpoint = %+v", ep)
	}
}
