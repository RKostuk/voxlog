package main

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"voxlog-go/internal/history"
	"voxlog-go/internal/mcp"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/task"
)

// testMCPApp builds the smallest app applyMCP can work with: the three
// stores it serves, plus a settings file of its own so the port it remembers
// cannot touch the real one.
func testMCPApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	return &app{
		store:    settings.NewStore(filepath.Join(dir, "settings.json")),
		hist:     history.NewStore(t.TempDir()),
		meetings: history.NewMeetingStore(t.TempDir()),
		tasks:    task.NewStore(t.TempDir()),
	}
}

func TestApplyMCPStartsAndStopsWithTheSetting(t *testing.T) {
	mcp.UseTokenDir(t.TempDir())
	t.Cleanup(func() { mcp.UseTokenDir("") })
	a := testMCPApp(t)

	a.applyMCP(settings.Settings{MCPEnabled: true})
	if a.mcpSrv == nil {
		t.Fatalf("the server did not start: %s", a.mcpErr)
	}
	port := a.mcpSrv.Port()
	if port == 0 {
		t.Fatal("the server reports no port")
	}

	a.applyMCP(settings.Settings{MCPEnabled: false})
	if a.mcpSrv != nil {
		t.Fatal("turning the setting off left the server running")
	}
	// Nothing is listening on that port any more, so binding it must succeed.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("the port is still held after the server stopped: %v", err)
	}
	ln.Close()
}

// The pane saves on every keystroke in the pane, so applyMCP is called far
// more often than anything changes. Rebinding each time would drop every
// connected client for nothing.
func TestApplyMCPWithAnUnchangedConfigDoesNotRebind(t *testing.T) {
	mcp.UseTokenDir(t.TempDir())
	t.Cleanup(func() { mcp.UseTokenDir("") })
	a := testMCPApp(t)
	t.Cleanup(func() { a.applyMCP(settings.Settings{}) })

	a.applyMCP(settings.Settings{MCPEnabled: true})
	if a.mcpSrv == nil {
		t.Fatalf("the server did not start: %s", a.mcpErr)
	}
	first := a.mcpSrv
	port := first.Port()

	a.applyMCP(settings.Settings{MCPEnabled: true, MCPPort: port})
	if a.mcpSrv != first {
		t.Fatal("an unchanged config rebound the listener")
	}
}

// An MCP client is configured once, so the address it was given has to keep
// working after a restart -- which means the port the kernel handed out is
// written back to the settings file.
func TestApplyMCPRemembersThePort(t *testing.T) {
	mcp.UseTokenDir(t.TempDir())
	t.Cleanup(func() { mcp.UseTokenDir("") })
	a := testMCPApp(t)
	t.Cleanup(func() { a.applyMCP(settings.Settings{}) })

	cfg := a.store.Get()
	cfg.MCPEnabled = true
	a.applyMCP(cfg)
	if a.mcpSrv == nil {
		t.Fatalf("the server did not start: %s", a.mcpErr)
	}
	if got := a.store.Get().MCPPort; got != a.mcpSrv.Port() {
		t.Fatalf("the settings remember port %d, the server is on %d", got, a.mcpSrv.Port())
	}
}

func TestMCPStatusSaysOffWhenItIsOff(t *testing.T) {
	a := testMCPApp(t)

	status := a.mcpStatus()
	if status["running"] != false || status["enabled"] != false {
		t.Fatalf("got %v, want a server that is neither enabled nor running", status)
	}
	if status["token"] != "" {
		t.Fatal("a token was handed out for a server that is not running")
	}
}
