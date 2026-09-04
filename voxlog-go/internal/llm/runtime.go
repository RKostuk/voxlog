// Runtime install: Task Hub's local classifier needs a Python + mlx-lm
// install, but that's ~150-230MB -- too heavy to ship in every copy of the
// app for a feature most users never turn on. So it's not bundled at all;
// EnsureRuntime builds it on the user's own machine, once, the first time
// Task Hub is enabled, using the same steps the Makefile's old vendor-mlx
// target used to run at build time (see git history) -- just run here
// instead of there.
package llm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const pythonVersion = "cpython-3.12"

// RuntimeDir is where the installed Python + mlx-lm lands, alongside the
// downloaded model under the same models base directory.
func RuntimeDir(baseDir string) string {
	return filepath.Join(baseDir, runtimeDirName)
}

func runtimePython(baseDir string) string {
	return filepath.Join(RuntimeDir(baseDir), "bin", "python")
}

func IsRuntimeInstalled(baseDir string) bool {
	info, err := os.Stat(runtimePython(baseDir))
	return err == nil && !info.IsDir()
}

// StatusFunc reports coarse install stages to the UI -- there's no
// meaningful byte-progress to show for "uv is resolving a CPython build" or
// "pip is compiling nothing because these are all prebuilt wheels", so this
// is text, not a percentage.
type StatusFunc func(stage string)

// EnsureRuntime installs the Python + mlx-lm runtime into baseDir if it
// isn't there already. Safe to call every time Task Hub starts; it's a
// no-op once installed.
func EnsureRuntime(baseDir string, status StatusFunc) error {
	if IsRuntimeInstalled(baseDir) {
		return nil
	}
	if status == nil {
		status = func(string) {}
	}

	uv, err := resolveUV(status)
	if err != nil {
		return fmt.Errorf("locate uv: %w", err)
	}

	dest := RuntimeDir(baseDir)
	tmpParent, err := os.MkdirTemp(baseDir, ".mlx-runtime-install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpParent)

	status("Installing Python…")
	if out, err := exec.Command(uv, "python", "install", pythonVersion, "--install-dir", tmpParent).CombinedOutput(); err != nil {
		return fmt.Errorf("uv python install: %w: %s", err, out)
	}

	installed, err := findInstalledPython(tmpParent)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(installed, dest); err != nil {
		return err
	}

	status("Installing mlx-lm…")
	pipCmd := exec.Command(uv, "pip", "install", "--python", filepath.Join(dest, "bin", "python"), "--break-system-packages", "mlx-lm")
	if out, err := pipCmd.CombinedOutput(); err != nil {
		os.RemoveAll(dest)
		return fmt.Errorf("uv pip install mlx-lm: %w: %s", err, out)
	}

	pruneRuntime(dest)

	// uv python install fetches the interpreter over the network, which on
	// macOS can leave com.apple.quarantine set on its executables --
	// Gatekeeper would otherwise prompt (or block outright, since this
	// binary isn't signed by us) the first time it's spawned as a
	// subprocess. -r because the interpreter package is a directory tree.
	if runtime.GOOS == "darwin" {
		exec.Command("xattr", "-dr", "com.apple.quarantine", dest).Run()
	}
	return nil
}

// findInstalledPython locates the one interpreter directory `uv python
// install --install-dir tmpParent` produced -- its name is versioned and
// platform-suffixed (e.g. cpython-3.12.7-macos-aarch64-none), not fixed.
func findInstalledPython(tmpParent string) (string, error) {
	entries, err := os.ReadDir(tmpParent)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(tmpParent, e.Name()), nil
		}
	}
	return "", fmt.Errorf("uv python install: no interpreter directory found under %s", tmpParent)
}

// pruneRuntime strips what running `python -m mlx_lm server` never touches
// -- Tcl/Tk/tkinter, headers, man pages -- mirroring the old Makefile
// vendor-mlx target's cleanup. Best-effort: a leftover file here is wasted
// disk, not a broken install, so errors are ignored.
func pruneRuntime(dest string) {
	for _, rel := range []string{
		"share", "include", "lib/pkgconfig",
		"lib/tcl8", "lib/tcl8.6", "lib/tk8.6",
		"lib/itcl4.2.4", "lib/thread2.8.9",
		"lib/libtcl8.6.dylib", "lib/libtk8.6.dylib",
		"lib/python3.12/tkinter",
		"lib/python3.12/lib-dynload/_tkinter.cpython-312-darwin.so",
	} {
		os.RemoveAll(filepath.Join(dest, rel))
	}
	binDir := filepath.Join(dest, "bin")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(e.Name()) >= 6 && e.Name()[:6] == "python" {
			continue
		}
		p := filepath.Join(binDir, e.Name())
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if _, err := os.Stat(p); err != nil {
				os.Remove(p) // dangling symlink
			}
			continue
		}
		os.Remove(p)
	}
}

// resolveUV finds an installed uv, or fetches astral's official installer
// script if none is on the machine yet. uv is only needed to BUILD the
// runtime once; the runtime itself doesn't depend on it afterward.
func resolveUV(status StatusFunc) (string, error) {
	if p, err := exec.LookPath("uv"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", "uv"),
		filepath.Join(home, ".cargo", "bin", "uv"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("no uv found and no installer for %s", runtime.GOOS)
	}
	status("Installing uv…")
	install := exec.Command("sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh")
	if out, err := install.CombinedOutput(); err != nil {
		return "", fmt.Errorf("install uv: %w: %s", err, out)
	}
	candidate := filepath.Join(home, ".local", "bin", "uv")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	return "", fmt.Errorf("uv installer ran but %s not found", candidate)
}
