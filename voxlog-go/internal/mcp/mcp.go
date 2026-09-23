// Package mcp serves Voxlog's notes, meetings and tasks to an LLM client
// over the Model Context Protocol.
//
// Streamable HTTP on loopback, and nothing else: the app is already running,
// so there is no process for a client to spawn and the switch in Settings is
// the real on/off. The server never initiates anything, so a call is a POST
// that gets one JSON-RPC response back -- no SSE stream, no session id, no
// state to leak between clients.
package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Config is what the settings pane decides.
type Config struct {
	// Port is the port to bind. Zero asks the kernel for one, which is what
	// the first ever start does; the caller is expected to remember what it
	// got, because an MCP client is configured once and has to find the same
	// address after a restart.
	Port int
	// AllowWrite is read on every tools/list and tools/call, so the write
	// switch takes effect without rebinding the listener.
	AllowWrite func() bool
}

// Server is one running listener.
type Server struct {
	ln   net.Listener
	http *http.Server
	port int

	closeOnce sync.Once
	closeErr  error
}

// Start binds and serves. A port that is already taken is not an error: the
// listener falls back to a kernel-assigned one and reports which, so the
// pane can tell the user to reconnect rather than leaving them with a server
// that silently never came up.
func Start(cfg Config, deps Deps, token string) (*Server, error) {
	if token == "" {
		return nil, fmt.Errorf("mcp: refusing to serve without a token")
	}

	ln, err := listen(cfg.Port)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/", newHandler(deps, token, cfg.AllowWrite))
	srv := &Server{
		ln:   ln,
		port: ln.Addr().(*net.TCPAddr).Port,
		// ReadHeaderTimeout: a loopback server still faces everything else on
		// the machine that can reach 127.0.0.1, and http.Serve's default of
		// no timeout leaves a slow-header connection open forever.
		http: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second},
	}
	go srv.http.Serve(ln)
	return srv, nil
}

// listen binds 127.0.0.1 explicitly -- never 0.0.0.0, which would put a
// user's transcripts on every network the machine is attached to.
func listen(port int) (net.Listener, error) {
	if port > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listen: %w", err)
	}
	return ln, nil
}

// Port is the port actually bound, which is not always the one asked for.
func (s *Server) Port() int { return s.port }

// URL is the address to hand a client.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/mcp", s.port) }

// Close stops serving. Idempotent, because the settings pane can ask for it
// more than once. The stores belong to the app and outlive this -- nothing
// here closes them.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.closeErr = s.http.Shutdown(ctx)
		s.ln.Close()
	})
	return s.closeErr
}
