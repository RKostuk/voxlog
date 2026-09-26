package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo is one model OpenRouter offers, as much of it as the settings
// pane shows.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

type modelsResponse struct {
	Data []struct {
		ModelInfo
		Architecture struct {
			OutputModalities []string `json:"output_modalities"`
		} `json:"architecture"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
	} `json:"data"`
}

// FreeModels lists the models at baseURL (OpenRouterBaseURL in the app, a
// test server in tests) that cost nothing to prompt or to answer, sorted by
// name. The catalogue is public, so no key is needed to fetch it. Free is
// read off the prices rather than the ":free" suffix: the price is what the
// user is actually promised.
func FreeModels(baseURL string) ([]ModelInfo, error) {
	url := strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + "/v1/models"
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s: %s", url, resp.Status, string(raw))
	}

	var parsed modelsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s: decoding model list: %w", url, err)
	}
	var out []ModelInfo
	for _, m := range parsed.Data {
		if m.Pricing.Prompt == "0" && m.Pricing.Completion == "0" && writesText(m.Architecture.OutputModalities) {
			out = append(out, m.ModelInfo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// writesText is false for the free models that answer in audio or images:
// they are on the free list, and useless for anything Voxlog asks. An entry
// that does not say is given the benefit of the doubt.
func writesText(modalities []string) bool {
	if len(modalities) == 0 {
		return true
	}
	for _, m := range modalities {
		if m == "text" {
			return true
		}
	}
	return false
}

// CheckOpenRouterKey says what is wrong with key as an OpenRouter key, or
// nil. OpenRouter answers a wrong one with "Missing Authentication header",
// which reads like a bug in the app rather than the wrong thing pasted --
// a link to the keys page, an OpenAI key -- so it is caught here first.
func CheckOpenRouterKey(key string) error {
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return fmt.Errorf("no OpenRouter key is stored for this account -- paste one from openrouter.ai/keys")
	case !strings.HasPrefix(key, "sk-or-"):
		prefix := key
		if len(prefix) > 6 {
			prefix = prefix[:6] + "…"
		}
		return fmt.Errorf("the stored key is not an OpenRouter key (it starts with %q; OpenRouter keys start with \"sk-or-\") -- replace it with one from openrouter.ai/keys", prefix)
	}
	return nil
}

// FreeQuota is how many free-model requests an OpenRouter key has left today.
// Limit is 50 without credits, 1000 with; the day is UTC's.
type FreeQuota struct {
	Used      int `json:"used"`
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
}

// KeyFreeQuota asks OpenRouter how much of today's free-model allowance key
// has left. Reading it costs nothing against that allowance.
func KeyFreeQuota(baseURL, key string) (FreeQuota, error) {
	url := strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + "/v1/key"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return FreeQuota{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return FreeQuota{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return FreeQuota{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return FreeQuota{}, fmt.Errorf("%s: %s: %s", url, resp.Status, string(raw))
	}
	var body struct {
		Data struct {
			Free *FreeQuota `json:"free_model_daily_requests"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return FreeQuota{}, fmt.Errorf("%s: decoding: %w", url, err)
	}
	if body.Data.Free == nil {
		return FreeQuota{}, fmt.Errorf("%s: no free_model_daily_requests in the reply", url)
	}
	return *body.Data.Free, nil
}
