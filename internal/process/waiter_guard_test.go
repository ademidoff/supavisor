package process

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// filesAllowedToSpawn are the only places that may start a child process.
// Anywhere else, os/exec is a second waiter waiting to happen.
//
// Test files are checked too. The reaper is process-wide, so a test binary that
// starts any program has the wait4 loop running in it, and a helper calling
// cmd.Wait() there is the same violation with the same silent failure.
var filesAllowedToSpawn = map[string]bool{
	"internal/process/process.go":           true,
	"internal/process/health.go":            true,
	"internal/process/reaper_test.go":       true,
	"internal/process/group_kill_test.go":   true,
	"internal/process/reaper_linux_test.go": true,
	"internal/server/state_test.go":         true,
}

// waitingMethods are the exec.Cmd methods that wait for the child themselves.
// Every one of them races the reaper for the same status.
//
// They are matched on the receiver rather than the bare method name, because
// Run and Output are far too common otherwise: t.Run alone would light up every
// table-driven test in the tree.
var waitingMethods = map[string]string{
	"Wait":           "the reaper is the only waiter; take the status from the channel spawn returned",
	"CombinedOutput": "it calls Wait internally",
	"Output":         "it calls Wait internally",
	"Run":            "it calls Wait internally",
}

// TestOnlyTheReaperWaitsForChildren enforces the invariant the whole design
// rests on: exactly one waiter in the daemon.
//
// wait4(-1) takes whatever has exited, so a second waiter steals statuses from
// the first. The failure is intermittent and reads as something else entirely —
// a health probe losing its status reports a healthy program as UNHEALTHY, and
// a program losing its status looks like it crashed when it exited cleanly.
// That is far too quiet to leave to review, and too rare to catch reliably by
// running the tests, so it is checked structurally instead.
//
// See probes/wait4-race/main.go for the measurement behind this.
func TestOnlyTheReaperWaitsForChildren(t *testing.T) {
	root := filepath.Join("..", "..")

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case info.IsDir() && (info.Name() == ".git" || info.Name() == "bin" || info.Name() == "probes"):
			return filepath.SkipDir
		case info.IsDir() || !strings.HasSuffix(path, ".go"):
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		checkFileDoesNotWait(t, path, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk the tree: %v", err)
	}
}

// receiverName reports the trailing identifier of a receiver expression, so
// that p.cmd and cmd both answer "cmd".
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

func checkFileDoesNotWait(t *testing.T, path, rel string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("Failed to parse %s: %v", rel, err)
	}

	for _, spec := range file.Imports {
		imported, convErr := strconv.Unquote(spec.Path.Value)
		if convErr != nil || imported != "os/exec" {
			continue
		}
		if !filesAllowedToSpawn[rel] {
			t.Errorf("%s imports os/exec. Starting a child anywhere else means a second waiter: "+
				"go through reaperFor(logger).spawn, or add this file to filesAllowedToSpawn "+
				"after making sure it never waits.", rel)
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line

		// exec.CommandContext is unambiguous: package-qualified, and banned
		// wherever it appears.
		if pkg, isIdent := selector.X.(*ast.Ident); isIdent && pkg.Name == "exec" && selector.Sel.Name == "CommandContext" {
			t.Errorf("%s:%d calls exec.CommandContext. Its context watcher is wired to Wait, and its Cancel "+
				"hook can fire against a reaped and recycled PID. Use exec.Command and handle the deadline "+
				"explicitly.", rel, line)
			return true
		}

		// Everything else is matched on the receiver: cmd.Wait(), p.cmd.Run(),
		// proc.cmd.CombinedOutput() are all the same violation.
		reason, forbidden := waitingMethods[selector.Sel.Name]
		if !forbidden || !strings.HasSuffix(strings.ToLower(receiverName(selector.X)), "cmd") {
			return true
		}
		t.Errorf("%s:%d calls %s on a command: %s. Start through reaperFor(logger).spawn and read the "+
			"status from the channel it returns.", rel, line, selector.Sel.Name, reason)
		return true
	})
}
