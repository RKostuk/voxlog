package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured is what the fake endpoint saw, so the two modes can be told apart
// by the request rather than by reading the code back.
type captured struct {
	auth string
	body chatRequest
}

func fakeEndpoint(t *testing.T, status int, reply string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("posted to %s, want /v1/chat/completions", r.URL.Path)
		}
		got.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got.body)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestLocalRequestsCarryNoKeyAndNoModel(t *testing.T) {
	srv, got := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	out, err := chatCompletion(Endpoint{BaseURL: srv.URL}, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi" {
		t.Errorf("reply = %q, want hi", out)
	}
	if got.auth != "" {
		t.Errorf("Authorization = %q, want none for the local server", got.auth)
	}
	// mlx_lm serves exactly the model it was started with; naming one is at
	// best ignored and at worst a 400.
	if got.body.Model != "" {
		t.Errorf("model = %q, want it omitted locally", got.body.Model)
	}
}

func TestAPIRequestsCarryTheKeyAndTheModel(t *testing.T) {
	srv, got := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	// A trailing slash on a pasted base URL must not produce a double slash.
	ep := Endpoint{BaseURL: srv.URL + "/", Model: "gpt-4o-mini", APIKey: "sk-secret"}
	if _, err := chatCompletion(ep, "prompt"); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", got.auth)
	}
	if got.body.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", got.body.Model)
	}
}

// "401 Unauthorized from the provider" and "the local model failed to load"
// need different fixes, so the error has to say which endpoint refused.
func TestErrorsNameTheEndpoint(t *testing.T) {
	srv, _ := fakeEndpoint(t, http.StatusUnauthorized, `{"error":"bad key"}`)

	_, err := chatCompletion(Endpoint{BaseURL: srv.URL, APIKey: "sk-wrong"}, "prompt")
	if err == nil {
		t.Fatal("a 401 came back as success")
	}
	if !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error = %q, want it to name the endpoint and quote the provider", err)
	}

	local, _ := fakeEndpoint(t, http.StatusInternalServerError, "boom")
	_, err = chatCompletion(Endpoint{BaseURL: local.URL}, "prompt")
	if err == nil {
		t.Fatal("a 500 came back as success")
	}
	if !strings.Contains(err.Error(), local.URL) {
		t.Errorf("error = %q, want it to name the endpoint", err)
	}
}

func TestPingSucceedsOnAnAnswerAndFailsOnSilence(t *testing.T) {
	ok, _ := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	if err := Ping(Endpoint{BaseURL: ok.URL, Model: "m", APIKey: "k"}); err != nil {
		t.Errorf("Ping on a working endpoint: %v", err)
	}

	empty, _ := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"  "}}]}`)
	if err := Ping(Endpoint{BaseURL: empty.URL}); err == nil {
		t.Error("Ping passed against an endpoint that answered with nothing")
	}
}

// What makes an endpoint remote is an address, not a label: a provider
// selected with no base URL behind it would send every summary nowhere.
func TestRemoteNeedsAnAddress(t *testing.T) {
	if (Endpoint{}).Remote() {
		t.Error("the zero endpoint reports itself remote")
	}
	if (Endpoint{BaseURL: "   ", Model: "gpt-4o-mini"}).Remote() {
		t.Error("a blank base URL reports itself remote")
	}
	if !(Endpoint{BaseURL: "https://api.openai.com"}).Remote() {
		t.Error("a configured endpoint does not report itself remote")
	}
	if got := (Endpoint{}).Label(); got != "mlx_lm server" {
		t.Errorf("local label = %q", got)
	}
}

// Fallback models travel as OpenRouter's "models" list, primary first, so a
// free model that is down or rate-limited hands over to the next one on the
// provider's side instead of failing the request here.
func TestFallbacksTravelAsModelsList(t *testing.T) {
	srv, got := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	ep := Endpoint{BaseURL: srv.URL, Model: "a:free", Fallbacks: []string{"b:free", " ", "a:free", "c:free"}}
	if _, err := chatCompletion(ep, "prompt"); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:free", "b:free", "c:free"}
	if strings.Join(got.body.Models, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", got.body.Models, want)
	}

	srv2, got2 := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	if _, err := chatCompletion(Endpoint{BaseURL: srv2.URL, Model: "gpt-4o-mini"}, "prompt"); err != nil {
		t.Fatal(err)
	}
	if got2.body.Models != nil {
		t.Errorf("models = %v, want it omitted with no fallbacks", got2.body.Models)
	}
}

// A 429 is the everyday failure of a free tier, and the provider's own words
// for it do not say what to do next.
func TestRateLimitSaysWhatToDo(t *testing.T) {
	srv, _ := fakeEndpoint(t, http.StatusTooManyRequests, `{"error":{"message":"Rate limit exceeded"}}`)
	_, err := chatCompletion(Endpoint{BaseURL: srv.URL, Model: "m", APIKey: "k"}, "prompt")
	if err == nil {
		t.Fatal("a 429 came back as success")
	}
	if !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "another account or model") {
		t.Errorf("error = %q, want it to say to switch account or model", err)
	}
}

// Every free model on OpenRouter thinks before it answers, and a budget
// sized for the answer alone ("ok" in 8 tokens) is spent entirely on the
// thinking -- the model then "answers" with nothing. So OpenRouter requests
// ask for no reasoning and leave room for it anyway.
func TestOpenRouterRequestsLeaveRoomForReasoning(t *testing.T) {
	got := &captured{}
	var title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("X-Title")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got.body)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	ep := Endpoint{BaseURL: srv.URL, Model: "m:free", openRouter: true}
	if err := Ping(ep); err != nil {
		t.Fatal(err)
	}
	if got.body.MaxTokens < minOpenRouterTokens {
		t.Errorf("max_tokens = %d, want at least %d", got.body.MaxTokens, minOpenRouterTokens)
	}
	if got.body.Reasoning == nil || got.body.Reasoning.Enabled {
		t.Errorf("reasoning = %+v, want it disabled", got.body.Reasoning)
	}
	if title != "Voxlog" {
		t.Errorf("X-Title = %q", title)
	}

	// Anyone else gets the request exactly as before.
	plain, got2 := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	if err := Ping(Endpoint{BaseURL: plain.URL}); err != nil {
		t.Fatal(err)
	}
	if got2.body.Reasoning != nil || got2.body.MaxTokens != 8 {
		t.Errorf("non-OpenRouter request changed: %+v", got2.body)
	}
}

// An empty reply cut off by the length limit is a model that spent its
// budget thinking; the error should say that rather than just "nothing".
func TestEmptyReplySaysWhy(t *testing.T) {
	srv, _ := fakeEndpoint(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}]}`)
	err := Ping(Endpoint{BaseURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %v, want it to mention the length cut-off", err)
	}
}

// OpenRouter models are asked one at a time, in the user's order, and a
// model that fails -- by status or by an answer that does not parse -- hands
// over to the next.
func TestAskInTurnFallsThroughTheModels(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		if body.Models != nil {
			t.Errorf("models = %v, want one model per request", body.Models)
		}
		asked = append(asked, body.Model)
		switch body.Model {
		case "a:free":
			w.WriteHeader(http.StatusTooManyRequests)
		case "b:free":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"not json"}}]}`)
		default:
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"tasks\":[]}"}}]}`)
		}
	}))
	defer srv.Close()

	ep := Endpoint{BaseURL: srv.URL, Model: "a:free", Fallbacks: []string{"b:free", "c:free"}, openRouter: true}
	var got string
	err := askInTurn(ep, "p", func(reply string) error {
		if _, err := extractJSONObject(reply); err != nil {
			return err
		}
		got = reply
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "a:free,b:free,c:free" || got != `{"tasks":[]}` {
		t.Errorf("asked %v, got %q", asked, got)
	}

	asked = nil
	if err := askInTurn(Endpoint{BaseURL: srv.URL, Model: "a:free", Fallbacks: []string{"b:free"}, openRouter: true}, "p", func(reply string) error { _, err := extractJSONObject(reply); return err }); err == nil {
		t.Error("every model failing came back as success")
	}
}

// OpenRouter's "free-models-per-day" 429 is the account running out, not a
// model: it is recognised with its reset time, and no further model is asked.
func TestDailyLimitStopsTheFallbacks(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"Rate limit exceeded: free-models-per-day.","code":429,"metadata":{"headers":{"X-RateLimit-Limit":"50","X-RateLimit-Remaining":"0","X-RateLimit-Reset":"1790380800000"},"limit_source":"openrouter_free_tier_daily"}}}`)
	}))
	defer srv.Close()

	ep := Endpoint{BaseURL: srv.URL, Model: "a:free", Fallbacks: []string{"b:free"}, openRouter: true}
	err := askInTurn(ep, "p", func(string) error { return nil })
	var limit *RateLimitError
	if !errors.As(err, &limit) || !limit.Daily {
		t.Fatalf("err = %v, want a daily RateLimitError", err)
	}
	if limit.Reset.UnixMilli() != 1790380800000 {
		t.Errorf("reset = %v", limit.Reset)
	}
	if asked != 1 {
		t.Errorf("asked %d models, want 1: the limit is the account's", asked)
	}
}
