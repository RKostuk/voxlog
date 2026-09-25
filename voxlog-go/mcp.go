package main

import (
	"log"

	"voxlog-go/internal/mcp"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/ui"
)

// applyMCP brings the MCP server into line with the settings it is handed.
//
// Idempotent: called once at startup and again after every save, so most
// calls find the world already the way it should be and do nothing. Settings
// are pushed in rather than read from the store, because settings.Store has
// no lock of its own and this runs off the window's goroutine.
func (a *app) applyMCP(cfg settings.Settings) {
	a.mcpMu.Lock()
	defer a.mcpMu.Unlock()

	if !cfg.MCPEnabled {
		if a.mcpSrv != nil {
			a.mcpSrv.Close()
			a.mcpSrv = nil
			log.Print("mcp: server stopped")
		}
		a.mcpErr = ""
		return
	}

	// Already up on the port that was asked for. A restart here would drop
	// every connected client for nothing.
	if a.mcpSrv != nil && (cfg.MCPPort == 0 || cfg.MCPPort == a.mcpSrv.Port()) {
		return
	}
	if a.mcpSrv != nil {
		a.mcpSrv.Close()
		a.mcpSrv = nil
	}

	token, err := mcp.LoadOrCreateToken()
	if err != nil {
		a.mcpErr = err.Error()
		log.Printf("mcp: %v", err)
		return
	}

	srv, err := mcp.Start(mcp.Config{
		Port: cfg.MCPPort,
		// A func, not the value: flipping the write switch has to take
		// effect without rebinding the listener.
		AllowWrite: func() bool { return a.store.Get().MCPAllowWrite },
	}, mcp.Deps{
		Notes:    a.hist,
		Meetings: a.meetings,
		Tasks:    a.tasks,
	}, token)
	if err != nil {
		a.mcpErr = err.Error()
		log.Printf("mcp: %v", err)
		notifyPane("Voxlog's MCP server could not start. See Settings.", ui.PaneSettings)
		return
	}

	a.mcpSrv = srv
	a.mcpErr = ""
	log.Printf("mcp: serving on %s", srv.URL())

	// Remember the port the kernel handed out, so the client configured
	// against this address still finds it after a restart. Read-modify-write
	// of the whole struct, the way every other writer of this store does it.
	if cfg.MCPPort != srv.Port() {
		current := a.store.Get()
		current.MCPPort = srv.Port()
		if err := a.store.Set(current); err != nil {
			log.Printf("mcp: remembering the port: %v", err)
		}
	}
}

// mcpStatus is what the settings pane shows. The token is in here because the
// pane's whole job is handing the user a command they can paste, and that
// command carries it.
func (a *app) mcpStatus() map[string]any {
	cfg := a.store.Get()

	a.mcpMu.Lock()
	srv, failure := a.mcpSrv, a.mcpErr
	a.mcpMu.Unlock()

	out := map[string]any{
		"enabled":    cfg.MCPEnabled,
		"allowWrite": cfg.MCPAllowWrite,
		"running":    srv != nil,
		"error":      failure,
		"url":        "",
		"port":       0,
		"token":      "",
	}
	if srv == nil {
		return out
	}
	out["url"] = srv.URL()
	out["port"] = srv.Port()
	// Only read once the server is up: a token file conjured into existence
	// by looking at a pane would be a credential nobody asked for.
	if token, err := mcp.LoadOrCreateToken(); err == nil {
		out["token"] = token
	}
	return out
}

// regenerateMCPToken issues a new token and returns it. The server keeps
// serving, on the new token -- every client holding the old one has to be
// added again, which is the point of the button.
func (a *app) regenerateMCPToken() (string, error) {
	token, err := mcp.RegenerateToken()
	if err != nil {
		return "", err
	}

	a.mcpMu.Lock()
	running := a.mcpSrv != nil
	port := 0
	if running {
		port = a.mcpSrv.Port()
		a.mcpSrv.Close()
		a.mcpSrv = nil
	}
	a.mcpMu.Unlock()

	if running {
		cfg := a.store.Get()
		cfg.MCPPort = port
		a.applyMCP(cfg)
	}
	return token, nil
}
