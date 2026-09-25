package ui

import (
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voxlog-go/internal/audio"
)

func testServer(t *testing.T) (*pageServer, string) {
	t.Helper()
	dir := t.TempDir()
	// A WAV header is not needed: nothing here decodes the bytes, and a
	// known-length body is what the Range assertions can actually check.
	if err := os.WriteFile(filepath.Join(dir, "take.wav"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := startPageServer(dir, nil)
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

// A voice's stored sample is the one piece of audio that does not come from a
// file, so it has its own route -- and the same token guard as everything
// else on this server.
func TestServesAVoiceClip(t *testing.T) {
	want := []byte("RIFF....WAVEfake")
	s, err := startPageServer(t.TempDir(), func(id int64) ([]byte, error) {
		if id != 7 {
			return nil, errNoSuchVoice
		}
		return want, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	resp, err := http.Get(s.VoiceURL() + "7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %s, want 200", resp.Status)
	}
	if got, _ := io.ReadAll(resp.Body); string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A voice with no sample, and a segment that is not an id at all, are
	// both simply not there.
	for _, path := range []string{"9", "not-a-number", ""} {
		resp, err := http.Get(s.VoiceURL() + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q: got %s, want 404", path, resp.Status)
		}
	}
}

// writeMeetingWAV puts a real recording in the directory: the mix endpoint
// parses both files, so the byte-blob the other tests use will not do.
func writeMeetingWAV(t *testing.T, dir, name string, samples []float32) {
	t.Helper()
	w, err := audio.NewWAVWriter(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(samples); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMixServesBothTracksAsOneRecording(t *testing.T) {
	s, dir := testServer(t)
	writeMeetingWAV(t, dir, "2026-09-24-101500-mic.wav", []float32{0.25, 0.25, 0.25, 0.25})
	writeMeetingWAV(t, dir, "2026-09-24-101500-system.wav", []float32{0.5, 0.5, 0.5, 0.5})

	res, err := http.Get(s.MixURL() + "2026-09-24-101500-mic.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "audio/wav" {
		t.Errorf("Content-Type %q, want audio/wav", got)
	}
	body, _ := io.ReadAll(res.Body)
	// 44-byte header plus two bytes a sample.
	if len(body) != 44+4*2 {
		t.Fatalf("got %d bytes, want %d", len(body), 44+4*2)
	}
	first := int16(binary.LittleEndian.Uint16(body[44:]))
	if want := int16(24575); first < want-8 || first > want+8 {
		t.Errorf("first sample is %d, want about %d (both tracks summed)", first, want)
	}
}

func TestMixSupportsRangeRequests(t *testing.T) {
	// The meeting timeline seeks into the middle of an hour-long call; that
	// has to be a range request, not a download.
	s, dir := testServer(t)
	writeMeetingWAV(t, dir, "call-mic.wav", []float32{0, 0, 0.5, 0.5})
	writeMeetingWAV(t, dir, "call-system.wav", []float32{0, 0, 0.25, 0.25})

	req, err := http.NewRequest(http.MethodGet, s.MixURL()+"call-mic.wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=44-47")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("got %d, want 206", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) != 4 {
		t.Fatalf("got %d bytes, want 4", len(body))
	}
}

func TestMixRefusesAPathAndAMissingRecording(t *testing.T) {
	s, _ := testServer(t)
	for _, name := range []string{"../../etc/hosts", "sub/take-mic.wav", "gone-mic.wav"} {
		res, err := http.Get(s.MixURL() + name)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%q: got %d, want 404", name, res.StatusCode)
		}
	}
}
