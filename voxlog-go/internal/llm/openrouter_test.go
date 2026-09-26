package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFreeModelsKeepsOnlyTheFreeOnes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("fetched %s, want /v1/models", r.URL.Path)
		}
		io.WriteString(w, `{"data":[
			{"id":"paid/big","name":"Big","context_length":200000,"pricing":{"prompt":"0.000003","completion":"0.000015"}},
			{"id":"z/zeta:free","name":"Zeta (free)","context_length":32768,"pricing":{"prompt":"0","completion":"0"}},
			{"id":"half/paid","name":"Half","context_length":8192,"pricing":{"prompt":"0","completion":"0.000001"}},
			{"id":"g/music","name":"Music","context_length":1000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["audio"]}},
			{"id":"a/alpha:free","name":"Alpha (free)","architecture":{"output_modalities":["text"]},"context_length":131072,"pricing":{"prompt":"0","completion":"0"}}
		]}`)
	}))
	defer srv.Close()

	got, err := FreeModels(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got), got)
	}
	if got[0].ID != "a/alpha:free" || got[1].ID != "z/zeta:free" {
		t.Errorf("order = %s, %s; want sorted by name", got[0].ID, got[1].ID)
	}
	if got[0].ContextLength != 131072 || got[0].Name != "Alpha (free)" {
		t.Errorf("first = %+v", got[0])
	}
}

func TestFreeModelsReportsAFailedFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := FreeModels(srv.URL); err == nil {
		t.Error("a 502 came back as an empty success")
	}
}

func TestCheckOpenRouterKey(t *testing.T) {
	if err := CheckOpenRouterKey("sk-or-v1-abc"); err != nil {
		t.Errorf("a real-looking key was refused: %v", err)
	}
	for _, bad := range []string{"", "https://openrouter.ai/keys", "sk-proj-abc"} {
		if err := CheckOpenRouterKey(bad); err == nil {
			t.Errorf("%q was accepted as an OpenRouter key", bad)
		}
	}
}

func TestKeyFreeQuota(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/key" {
			t.Errorf("asked %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"data":{"is_free_tier":true,"free_model_daily_requests":{"used":4,"limit":50,"remaining":46}}}`)
	}))
	defer srv.Close()
	q, err := KeyFreeQuota(srv.URL, "sk-or-x")
	if err != nil {
		t.Fatal(err)
	}
	if q != (FreeQuota{Used: 4, Limit: 50, Remaining: 46}) || auth != "Bearer sk-or-x" {
		t.Errorf("got %+v, auth %q", q, auth)
	}
}
