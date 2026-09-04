// Package llm runs Task Hub's local classifier: mlx_lm's HTTP server (Apple's
// MLX runtime -- the model is an MLX-quantized build, not GGUF) as a
// subprocess, spoken to over its OpenAI-compatible API on loopback. The
// subprocess is a relocatable, self-contained Python + mlx-lm install, built
// on the user's machine the first time Task Hub is enabled (see
// EnsureRuntime in runtime.go) rather than shipped in the app bundle --
// nothing for most users to pay for if they never turn the feature on.
package llm

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// idleTimeout is how long the subprocess stays resident after the last
// classification before it's shut down. Long enough that a burst of
// dictations doesn't repeatedly pay the several-second spawn+model-load
// cost; short enough that an idle machine doesn't hold a multi-gigabyte
// model in RAM indefinitely. Not load-bearing -- easy to retune.
const idleTimeout = 8 * time.Minute

// Cache holds one running mlx_lm server subprocess, spawned lazily on first
// use and evicted after idleTimeout of no classification calls. Mirrors
// main.go's transcriberCache (lazy, cached, rebuilt on config change) with
// idle eviction added on top: a resident LLM is a much heavier steady-state
// RAM cost than the ASR model, and this app is a background menu-bar process
// that should not hold 4.5GB+ resident once the user has stopped dictating.
type Cache struct {
	mu        sync.Mutex
	baseDir   string
	cmd       *exec.Cmd
	port      int
	ready     bool
	idleTimer *time.Timer
}

// NewCache scopes a Cache to baseDir, the same models directory Task Hub's
// model download uses -- EnsureRuntime installs the Python runtime as a
// sibling of the downloaded model files there.
func NewCache(baseDir string) *Cache { return &Cache{baseDir: baseDir} }

// runtimeDirName is the installed runtime's directory name under baseDir.
const runtimeDirName = "mlx-runtime"

// PythonPath resolves the interpreter that runs mlx_lm.server, installed by
// EnsureRuntime under baseDir/mlx-runtime/bin/python. An unbundled dev build
// falls back to $PATH, on the assumption a developer running from source has
// mlx-lm installed globally.
func PythonPath(baseDir string) (string, error) {
	candidate := runtimePython(baseDir)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	return exec.LookPath("python3")
}

// baseURL returns the running server's address, spawning it first if it
// isn't already up. Concurrent callers block on mu -- classification runs
// one at a time, which is fine for a background feature that never blocks a
// hotkey-driven interaction.
func (c *Cache) baseURL(modelDir string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil && c.ready {
		c.resetIdleTimerLocked()
		return fmt.Sprintf("http://127.0.0.1:%d", c.port), nil
	}

	if err := EnsureRuntime(c.baseDir, nil); err != nil {
		return "", fmt.Errorf("install mlx-lm runtime: %w", err)
	}
	python, err := PythonPath(c.baseDir)
	if err != nil {
		return "", fmt.Errorf("mlx-lm runtime not found: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(python, "-m", "mlx_lm", "server",
		"--model", modelDir,
		"--port", fmt.Sprintf("%d", port),
		"--host", "127.0.0.1",
		"--temp", "0",
		// Qwen3's chat template emits a <think>...</think> reasoning block
		// before the answer unless this is off -- classify.go parses the
		// reply as JSON, and a leading thinking block would break that.
		"--chat-template-args", `{"enable_thinking":false}`,
	)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start mlx_lm server: %w", err)
	}

	// Loading a multi-gigabyte model off disk plus Metal shader compilation
	// on first run can take a while -- longer than llama.cpp's plain mmap,
	// so the health-check budget is correspondingly longer.
	if err := waitHealthy(port, 90*time.Second); err != nil {
		cmd.Process.Kill()
		return "", err
	}

	c.cmd, c.port, c.ready = cmd, port, true
	c.resetIdleTimerLocked()
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

func (c *Cache) resetIdleTimerLocked() {
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.idleTimer = time.AfterFunc(idleTimeout, c.evict)
}

func (c *Cache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shutdownLocked()
}

func (c *Cache) shutdownLocked() {
	if c.cmd == nil || c.cmd.Process == nil {
		c.cmd, c.ready = nil, false
		return
	}
	proc := c.cmd.Process
	proc.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	cmd := c.cmd
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		proc.Kill()
	}
	c.cmd, c.ready = nil, false
}

// Shutdown stops the subprocess if one is running. Call on app quit so the
// mlx_lm server is never left orphaned.
func (c *Cache) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.shutdownLocked()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("mlx_lm server did not become healthy within %s", timeout)
}
