package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	// Model is omitted for the local mlx_lm server, which serves exactly the
	// one model it was started with and needs no naming. Every remote
	// OpenAI-compatible endpoint requires it.
	Model       string        `json:"model,omitempty"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Endpoint is where a prompt is sent. Zero value means the local mlx_lm
// server this package starts and owns (see server.go); a BaseURL makes it
// somebody else's OpenAI-compatible API, which is the whole of what
// "use an API key instead of a local model" needs -- the request shape is
// already the same one.
type Endpoint struct {
	BaseURL string
	// Model is the provider's model id. Required remotely, meaningless
	// locally.
	Model string
	// APIKey travels as a bearer token and is never stored in settings.json
	// -- it lives in the keychain (internal/keychain).
	APIKey string
}

// Remote reports whether this endpoint is somebody else's server. What makes
// it remote is a base URL the user typed, not a provider label: a label with
// no address behind it would send every summary nowhere.
func (e Endpoint) Remote() bool { return strings.TrimSpace(e.BaseURL) != "" }

// Label is what an error message calls this endpoint. "mlx_lm server" was
// baked into every error here, which is a lie as soon as the request goes to
// a provider the user configured -- and "the provider rejected your key"
// versus "the local model failed to load" are the two things those errors
// have to tell apart.
func (e Endpoint) Label() string {
	if e.Remote() {
		return strings.TrimSuffix(strings.TrimSpace(e.BaseURL), "/")
	}
	return "mlx_lm server"
}

// chatCompletion posts one prompt to an OpenAI-compatible chat endpoint and
// returns the model's reply text. Neither the local mlx_lm server nor most
// remote providers offer grammar/JSON-schema-constrained decoding (unlike
// llama-server), so the prompt itself asks for the shape it wants and
// classify.go/summarize.go parse leniently rather than trusting it.
func chatCompletion(ep Endpoint, prompt string) (string, error) {
	return chat(ep, prompt, 300)
}

func chat(ep Endpoint, prompt string, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       ep.Model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", err
	}

	url := strings.TrimSuffix(strings.TrimSpace(ep.BaseURL), "/") + "/v1/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Only when there is a key: the local server takes no auth, and sending
	// it an empty bearer token is a header that can only confuse a proxy.
	if key := strings.TrimSpace(ep.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s: %s", ep.Label(), resp.Status, string(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s: decoding response: %w", ep.Label(), err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: empty response", ep.Label())
	}
	return out.Choices[0].Message.Content, nil
}

// Ping sends the smallest possible request, so the settings pane can say
// whether an endpoint works while the user is looking at it. Without it the
// first sign of a wrong key or a typo in the base URL is a meeting an hour
// later with no summary and a line in the log.
func Ping(ep Endpoint) error {
	reply, err := chat(ep, "Reply with the single word: ok", 8)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) == "" {
		return fmt.Errorf("%s: connected, but the model returned nothing", ep.Label())
	}
	return nil
}
