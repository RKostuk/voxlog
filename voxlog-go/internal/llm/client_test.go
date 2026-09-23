package llm

import (
	"encoding/json"
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
