package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
	Model string `json:"model,omitempty"`
	// Models is OpenRouter's fallback list: the primary model first, then the
	// ones to try when it is down or rate-limited. Other providers never see
	// it -- it is only filled when Endpoint.Fallbacks is.
	Models []string `json:"models,omitempty"`
	// Reasoning is OpenRouter's switch for a model's thinking; nil (and
	// absent) everywhere else.
	Reasoning   *reasoningOpts `json:"reasoning,omitempty"`
	Messages    []chatMessage  `json:"messages"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens"`
}

type reasoningOpts struct {
	Enabled bool `json:"enabled"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

// minOpenRouterTokens is the least any OpenRouter request is given. Every
// free model there reasons before answering, and some cannot be told not
// to; a budget sized for the answer alone (8 tokens for Ping's "ok") is then
// spent on the thinking and the reply comes back empty. Free models cost
// nothing per token, so the room is free too.
const minOpenRouterTokens = 2048

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
	// -- it lives in its own file (internal/secrets).
	APIKey string
	// Fallbacks are further model ids for the provider to try, in order,
	// when Model cannot answer. Only OpenRouter reads them (as "models").
	Fallbacks []string
	// openRouter marks an endpoint as OpenRouter regardless of its address,
	// so tests can point one at a local server.
	openRouter bool
}

// OpenRouterBaseURL is OpenRouter's OpenAI-compatible root; chat appends
// "/v1/chat/completions" like it does for every other endpoint.
const OpenRouterBaseURL = "https://openrouter.ai/api"

// modelList is Model followed by the distinct, non-blank Fallbacks, or nil
// when there are no fallbacks -- a one-model list is just Model said twice.
func (e Endpoint) modelList() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append([]string{e.Model}, e.Fallbacks...) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

func (e Endpoint) isOpenRouter() bool {
	return e.openRouter || strings.HasPrefix(strings.TrimSpace(e.BaseURL), OpenRouterBaseURL)
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
	req := chatRequest{
		Model:       ep.Model,
		Models:      ep.modelList(),
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   maxTokens,
	}
	if ep.isOpenRouter() {
		// Thinking adds latency and nothing a classification needs; where a
		// model insists on it anyway, the budget floor leaves it room.
		req.Reasoning = &reasoningOpts{Enabled: false}
		if req.MaxTokens < minOpenRouterTokens {
			req.MaxTokens = minOpenRouterTokens
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	url := strings.TrimSuffix(strings.TrimSpace(ep.BaseURL), "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Only when there is a key: the local server takes no auth, and sending
	// it an empty bearer token is a header that can only confuse a proxy.
	if key := strings.TrimSpace(ep.APIKey); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	if ep.isOpenRouter() {
		// OpenRouter's attribution header: optional, and it makes the
		// requests recognisable on the user's own activity page.
		httpReq.Header.Set("X-Title", "Voxlog")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", newRateLimitError(ep, resp.Header, raw)
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
	content := out.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" && out.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("%s: the model used its whole token budget (finish_reason: length) without answering -- it may be a reasoning model; try another", ep.Label())
	}
	return content, nil
}

// candidates is ep split into one endpoint per model, in the user's order:
// the chosen model, then each fallback. Anything that is not OpenRouter with
// fallbacks is just itself.
func (e Endpoint) candidates() []Endpoint {
	models := e.modelList()
	if !e.isOpenRouter() || models == nil {
		return []Endpoint{e}
	}
	out := make([]Endpoint, len(models))
	for i, m := range models {
		one := e
		one.Model, one.Fallbacks = m, nil
		out[i] = one
	}
	return out
}

// askInTurn sends prompt to each of ep's models in order until one gives a
// reply that accept takes. OpenRouter's own "models" fallback only covers a
// model that is down or rate-limited; a free model that answers with prose,
// nothing, or broken JSON counts there as a success. Here it is a reason to
// ask the next one.
func askInTurn(ep Endpoint, prompt string, accept func(reply string) error) error {
	var errs []error
	for _, one := range ep.candidates() {
		reply, err := chatCompletion(one, prompt)
		if err == nil {
			err = accept(reply)
		}
		if err == nil {
			return nil
		}
		// The daily free-model allowance is the account's, not the model's:
		// every other model would be refused the same way.
		var limit *RateLimitError
		stop := errors.As(err, &limit) && limit.Daily
		if one.Model != "" {
			err = fmt.Errorf("%s: %w", one.Model, err)
		}
		log.Printf("llm: %v", err)
		errs = append(errs, err)
		if stop {
			break
		}
	}
	return errors.Join(errs...)
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

// RateLimitError is a 429. Daily is OpenRouter's free-model allowance for the
// day (50 requests without credits) running out -- the account is done until
// Reset, whichever model is asked. Otherwise it is a short burst limit on one
// model, and the next one may well answer.
type RateLimitError struct {
	Label string
	Daily bool
	// Reset is when the allowance comes back; zero if the reply did not say.
	Reset time.Time
	Body  string
}

func (e *RateLimitError) Error() string {
	if e.Daily {
		when := ""
		if !e.Reset.IsZero() {
			when = " until " + e.Reset.Local().Format("15:04")
		}
		return fmt.Sprintf("%s: the free daily limit is used up%s -- switch to another account", e.Label, when)
	}
	// The everyday failure of a free tier, and the provider's own words for
	// it do not say what to do about it.
	return fmt.Sprintf("%s: rate limit reached -- try again later, or switch to another account or model: %s", e.Label, e.Body)
}

// newRateLimitError reads OpenRouter's 429: the reset time travels both as a
// response header and inside the error body's metadata, in milliseconds.
func newRateLimitError(ep Endpoint, h http.Header, raw []byte) *RateLimitError {
	e := &RateLimitError{Label: ep.Label(), Body: string(raw)}
	var body struct {
		Error struct {
			Message  string `json:"message"`
			Metadata struct {
				Headers     map[string]string `json:"headers"`
				LimitSource string            `json:"limit_source"`
			} `json:"metadata"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &body)
	e.Daily = strings.Contains(body.Error.Metadata.LimitSource, "daily") ||
		strings.Contains(body.Error.Message, "per-day")
	reset := h.Get("X-RateLimit-Reset")
	if reset == "" {
		reset = body.Error.Metadata.Headers["X-RateLimit-Reset"]
	}
	if ms, err := strconv.ParseInt(reset, 10, 64); err == nil && ms > 0 {
		e.Reset = time.UnixMilli(ms)
	}
	return e
}
