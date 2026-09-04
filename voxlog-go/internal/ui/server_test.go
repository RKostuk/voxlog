package ui

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) (*pageServer, string) {
	t.Helper()
	dir := t.TempDir()
	// A WAV header is not needed: nothing here decodes the bytes, and a
	// known-length body is what the Range assertions can actually check.
	if err := os.WriteFile(filepath.Join(dir, "take.wav"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := startPageServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestPageIsServedOnlyWithTheToken(t *testing.T) {
	s, _ := testServer(t)

	res, err := http.Get(s.PageURL())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d for the real page URL, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `id="shell-nav"`) {
		t.Error("the page served is not the main window")
	}

	// A wrong token must look exactly like a URL that was never a route:
	// 403 would confirm that some other token is the right one.
	bad := strings.Replace(s.PageURL(), s.token, "0000000000000000", 1)
	res2, err := http.Get(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d for a wrong token, want 404", res2.StatusCode)
	}
}

func TestAudioIsServedFromTheRecordingsDirectory(t *testing.T) {
	s, _ := testServer(t)

	res, err := http.Get(s.AudioURL() + "take.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "0123456789" {
		t.Fatalf("got %q, want the file's bytes", body)
	}
}

// The token lives in the URL; a Referrer-Policy header keeps it from ever
// riding an outbound Referer if a later phase adds an external link.
func TestPageResponseRefusesToLeakTheTokenAsAReferrer(t *testing.T) {
	s, _ := testServer(t)

	res, err := http.Get(s.PageURL())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// Seeking in an hour-long meeting is the whole reason this goes over HTTP
// rather than through a binding.
func TestAudioSupportsRangeRequests(t *testing.T) {
	s, _ := testServer(t)

	req, _ := http.NewRequest("GET", s.AudioURL()+"take.wav", nil)
	req.Header.Set("Range", "bytes=3-5")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("got %d, want 206", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "345" {
		t.Fatalf("got %q, want the requested range", body)
	}
}

// The handler turns a URL into a filesystem read. Everything below must stay
// inside the recordings directory.
func TestAudioRefusesAnythingOutsideTheRecordingsDirectory(t *testing.T) {
	s, dir := testServer(t)
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })

	for _, name := range []string{
		"../secret.txt",
		"..%2Fsecret.txt",
		"sub/../../secret.txt",
		"/etc/hosts",
		"",
	} {
		res, err := http.Get(s.AudioURL() + name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("%q was served with 200 and body %q; it must be refused", name, body)
		}
		if strings.Contains(string(body), "not yours") {
			t.Errorf("%q reached a file outside the recordings directory", name)
		}
	}
}

func TestAudioMissingFileIs404(t *testing.T) {
	s, _ := testServer(t)
	res, err := http.Get(s.AudioURL() + "nope.wav")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.StatusCode)
	}
}

// Two windows in one process would fight over a fixed port, and a fixed port
// is also the one an unrelated process could be squatting on.
func TestTwoServersGetDifferentPorts(t *testing.T) {
	a, _ := testServer(t)
	b, _ := testServer(t)
	if a.PageURL() == b.PageURL() {
		t.Fatalf("both servers claim %s", a.PageURL())
	}
}
