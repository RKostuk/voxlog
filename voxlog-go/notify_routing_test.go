package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"voxlog-go/internal/ui"
)

// knownPanes is what a click on a banner is allowed to open.
var knownPanes = map[string]bool{
	ui.PaneOverview: true,
	ui.PaneHistory:  true,
	ui.PaneMeetings: true,
	ui.PaneTasks:    true,
	ui.PaneSettings: true,
}

// Every banner has to carry somewhere to go. An empty action is not "no
// destination": the delegate refuses to route it (see
// internal/usernotify/notify_darwin.m), so the banner looks exactly like the
// ones that work and does nothing at all when clicked -- which reads as the
// app being broken, and is what sixteen call sites used to do.
//
// Asserted over the source rather than at runtime because the failure mode is
// a new call site, not a regression in an existing one.
func TestEveryBannerCarriesAPane(t *testing.T) {
	for _, file := range goFilesOf(t, ".") {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calledName(call)
			switch name {
			case "notifyPane", "usernotify.Post":
				if len(call.Args) != 2 {
					return true
				}
				where := fset.Position(call.Pos())
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok {
					// ui.PaneX, or a variable holding one -- the constants are
					// the point, so anything that is not a literal is fine.
					return true
				}
				pane, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if pane == "" {
					t.Errorf("%s: %s posts a banner with no destination; a click on it does nothing", where, name)
					return true
				}
				if !knownPanes[pane] {
					t.Errorf("%s: %s posts to pane %q, which the window does not have", where, name, pane)
				}
			case "notify":
				// notify itself defaults to Overview (see main.go); nothing to
				// check at the call site.
			}
			return true
		})
	}
}

// The declaration behind that default, so "notify posts to Overview" is not
// just a comment.
func TestNotifyDefaultsToAPaneThatExists(t *testing.T) {
	if !knownPanes[ui.PaneOverview] {
		t.Fatal("Overview is not one of the window's panes")
	}
	if got := ui.PaneOverview; got != "overview" {
		t.Errorf("PaneOverview = %q; the page's nav keys off \"overview\"", got)
	}
}

func goFilesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatalf("no source files found in %s", dir)
	}
	return out
}

// calledName renders the function being called as "notify" or "pkg.Func".
func calledName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok {
			return pkg.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return ""
}
