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
			{"id":"a/alpha:free","name":"Alpha (free)","context_length":131072,"pricing":{"prompt":"0","completion":"0"}}
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
