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
		if m.Pricing.Prompt == "0" && m.Pricing.Completion == "0" {
			out = append(out, m.ModelInfo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}
