package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"voxlog-go/internal/audio"
)

// pageServer serves the main window over loopback. The token in the URL path
// is what keeps it private: anything on the machine that can reach
// 127.0.0.1 but doesn't know the token gets a 404, not a window into a
// user's recordings.
type pageServer struct {
	ln    net.Listener
	token string
}

// startPageServer binds 127.0.0.1 on a kernel-assigned port and starts
// serving. recordingsDir is the only directory audio may be read from.
func startPageServer(recordingsDir string, voiceClip func(int64) ([]byte, error)) (*pageServer, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	s := &pageServer{ln: ln, token: token}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle(recordingsDir, voiceClip))
	// ReadHeaderTimeout: a loopback server still faces anything else on the
	// machine that can reach 127.0.0.1, and the bare http.Serve default of
	// no timeout at all leaves a slow-header connection open forever.
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go httpSrv.Serve(ln)

	return s, nil
}

// handle checks the token before anything else in the path is trusted: a
// mismatched token must 404 exactly like a route that was never registered,
// so a caller can't tell "wrong token" from "no such page" by status code.
func (s *pageServer) handle(recordingsDir string, voiceClip func(int64) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest, ok := splitToken(r.URL.Path, s.token)
		if !ok {
			http.NotFound(w, r)
			return
		}

		switch {
		case rest == "/" || rest == "":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// The token lives in the URL; no outbound link on this page
			// today, but Referrer-Policy keeps it that way for the next one.
			w.Header().Set("Referrer-Policy", "no-referrer")
			fmt.Fprint(w, buildMainPage())
		case strings.HasPrefix(rest, "/audio/"):
			serveAudio(w, r, recordingsDir, strings.TrimPrefix(rest, "/audio/"))
		case strings.HasPrefix(rest, "/mix/"):
			// Both sides of a meeting as one stream. Named by the microphone
			// file, because that is the one a meeting always has.
			serveMix(w, r, recordingsDir, strings.TrimPrefix(rest, "/mix/"))
		case strings.HasPrefix(rest, "/voice/"):
			// A voice's stored sample, which is the one piece of audio that
			// lives in the database rather than on disk -- so that naming
			// somebody keeps working after retention sweeps the call it came
			// from. No path is involved, so no traversal is possible: the
			// segment is an id or it is nothing.
			serveVoiceClip(w, r, voiceClip, strings.TrimPrefix(rest, "/voice/"))
		default:
			http.NotFound(w, r)
		}
	}
}

// splitToken pulls the first path segment off and compares it against token
// in constant time, so a timing side channel can't help an attacker guess it
// one byte at a time.
func splitToken(path, token string) (rest string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	seg, rest, _ := strings.Cut(path, "/")
	if subtle.ConstantTimeCompare([]byte(seg), []byte(token)) != 1 {
		return "", false
	}
	return "/" + rest, true
}

// serveAudio reads name out of recordingsDir and nowhere else. The handler
// turns a URL straight into a filesystem read, so every step here exists to
// keep that read inside recordingsDir.
func serveAudio(w http.ResponseWriter, r *http.Request, recordingsDir, name string) {
	if name == "" {
		http.NotFound(w, r)
		return
	}
	// filepath.Base strips any directory component, so this single
	// comparison catches "..", "sub/x", and an absolute path like
	// "/etc/hosts" all at once: none of them equal their own base name.
	if name != filepath.Base(name) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(filepath.Join(recordingsDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	http.ServeContent(w, r, name, stat.ModTime(), f)
}

// serveMix answers with the microphone and the call summed into one WAV,
// made as it is read (see audio.OpenMix). The name in the URL is the mic
// file; its -system sibling is found from it rather than passed in, so a URL
// can only ever name a meeting's own two halves.
//
// This is what "play the meeting" means: handed the mic file alone, a player
// plays the user talking to nobody.
func serveMix(w http.ResponseWriter, r *http.Request, recordingsDir, name string) {
	if name == "" || name != filepath.Base(name) {
		http.NotFound(w, r)
		return
	}
	micPath := filepath.Join(recordingsDir, name)
	stat, err := os.Stat(micPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sysPath := ""
	if base, ok := strings.CutSuffix(micPath, "-mic.wav"); ok {
		sysPath = base + "-system.wav"
	}

	mix, err := audio.OpenMix(micPath, sysPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer mix.Close()

	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, name, stat.ModTime(), mix)
}

// serveVoiceClip writes one voice's stored sample. Small enough (a couple of
// seconds of mono 16 kHz) that it is written whole rather than served with
// ranges, unlike a meeting recording.
func serveVoiceClip(w http.ResponseWriter, r *http.Request, voiceClip func(int64) ([]byte, error), idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || voiceClip == nil {
		http.NotFound(w, r)
		return
	}
	clip, err := voiceClip(id)
	if err != nil || len(clip) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(clip)
}

// PageURL is the main window's address, token included.
func (s *pageServer) PageURL() string {
	return fmt.Sprintf("http://%s/%s/", s.ln.Addr().String(), s.token)
}

// VoiceURL is the prefix a voice's id is appended to.
func (s *pageServer) VoiceURL() string {
	return fmt.Sprintf("http://%s/%s/voice/", s.ln.Addr().String(), s.token)
}

// AudioURL is the prefix a recording's file name is appended to.
func (s *pageServer) AudioURL() string {
	return fmt.Sprintf("http://%s/%s/audio/", s.ln.Addr().String(), s.token)
}

// MixURL is the prefix a meeting's microphone file name is appended to, for
// the two tracks played as one.
func (s *pageServer) MixURL() string {
	return fmt.Sprintf("http://%s/%s/mix/", s.ln.Addr().String(), s.token)
}

// Close shuts the listener down. Only the test calls this today, but a
// server that cannot be stopped is untestable.
func (s *pageServer) Close() error {
	return s.ln.Close()
}
