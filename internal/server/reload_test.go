package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/process"
)

// newReloadServer writes a config file and builds a server from it, returning
// the server and a function that rewrites the file.
func newReloadServer(t *testing.T, content string) (sv *Server, rewrite func(string)) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("/tmp", "sv-reload")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	configPath := filepath.Join(tmpDir, "supavisor.yml")
	write := func(body string) {
		header := "supavisor:\n  pidfile: " + filepath.Join(tmpDir, "sv.pid") +
			"\n  socket: " + filepath.Join(tmpDir, "s.sock") + "\n"
		if err := os.WriteFile(configPath, []byte(header+body), 0o644); err != nil {
			t.Fatalf("Failed to write config: %v", err)
		}
	}
	write(content)

	cfg, err := config.ParseConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}
	sv, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return sv, write
}

func TestReload_AddsAndRemovesPrograms(t *testing.T) {
	sv, write := newReloadServer(t, `
programs:
  keep:
    command: /bin/sleep 60
    startsecs: 1
  goaway:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "keep", process.StateRunning, 10*time.Second)
	waitForState(t, sv, "goaway", process.StateRunning, 10*time.Second)
	keepPID := sv.process("keep").GetPID()

	write(`
programs:
  keep:
    command: /bin/sleep 60
    startsecs: 1
  brandnew:
    command: /bin/sleep 60
    startsecs: 1
`)
	applied, err := sv.Reload()
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if !slices.Equal(applied.Added, []string{"brandnew"}) {
		t.Errorf("Reload should report what it added, got %v", applied.Added)
	}
	if !slices.Equal(applied.Removed, []string{"goaway"}) {
		t.Errorf("Reload should report what it removed, got %v", applied.Removed)
	}
	if len(applied.Changed) != 0 {
		t.Errorf("Nothing was redefined, got changed %v", applied.Changed)
	}

	if sv.process("goaway") != nil {
		t.Error("A removed program should be gone after reload")
	}
	if sv.process("brandnew") == nil {
		t.Fatal("An added program should be present after reload")
	}
	waitForState(t, sv, "brandnew", process.StateRunning, 10*time.Second)

	// An untouched program must not be disturbed.
	if got := sv.process("keep").GetPID(); got != keepPID {
		t.Errorf("Unchanged program was restarted: pid %d -> %d", keepPID, got)
	}
	if state := sv.process("keep").GetState(); state != process.StateRunning {
		t.Errorf("Unchanged program is %s after reload", state)
	}
}

func TestReload_RestartsAProgramWhoseDefinitionChanged(t *testing.T) {
	sv, write := newReloadServer(t, `
programs:
  app:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "app", process.StateRunning, 10*time.Second)
	before := sv.process("app").GetPID()

	write(`
programs:
  app:
    command: /bin/sleep 120
    startsecs: 1
`)
	applied, err := sv.Reload()
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if !slices.Equal(applied.Changed, []string{"app"}) {
		t.Errorf("Reload should report the redefined program, got %v", applied.Changed)
	}
	waitForState(t, sv, "app", process.StateRunning, 10*time.Second)

	if after := sv.process("app").GetPID(); after == before {
		t.Errorf("Changed program still on the old process %d", after)
	}
	if cmd := sv.config.Programs["app"].Command; !strings.Contains(cmd, "120") {
		t.Errorf("Reload did not apply the new command, got: %s", cmd)
	}
}

// TestReload_KeepsAStoppedProgramStopped checks that reload respects what an
// operator asked for rather than resetting everything to autostart.
func TestReload_KeepsAStoppedProgramStopped(t *testing.T) {
	sv, write := newReloadServer(t, `
programs:
  app:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "app", process.StateRunning, 10*time.Second)
	if err := sv.StopProcess("app"); err != nil {
		t.Fatalf("StopProcess failed: %v", err)
	}

	write(`
programs:
  app:
    command: /bin/sleep 120
    startsecs: 1
`)
	if _, err := sv.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	time.Sleep(2 * time.Second)
	if state := sv.process("app").GetState(); state != process.StateStopped {
		t.Errorf("Reload restarted a deliberately stopped program, it is %s", state)
	}
}

func TestReload_RejectsABrokenConfig(t *testing.T) {
	sv, write := newReloadServer(t, `
programs:
  app:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })
	waitForState(t, sv, "app", process.StateRunning, 10*time.Second)

	write(`
programs:
  app:
    command: /bin/sleep 60
    thisKeyIsNonsense: true
`)
	if _, err := sv.Reload(); err == nil {
		t.Fatal("Expected a broken config to be refused")
	}

	// The running configuration must be untouched by a failed reload.
	if state := sv.process("app").GetState(); state != process.StateRunning {
		t.Errorf("A failed reload disturbed the running program, it is %s", state)
	}
}

func TestReload_RejectsChangedDaemonSettings(t *testing.T) {
	sv, _ := newReloadServer(t, `
programs:
  app:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// Rewrite with a different socket, which is bound at startup
	body := "supavisor:\n  pidfile: " + sv.config.Supavisor.PidFile +
		"\n  socket: /tmp/some-other.sock\nprograms:\n  app:\n    command: /bin/sleep 60\n    startsecs: 1\n"
	if err := os.WriteFile(sv.config.SourcePath, []byte(body), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	_, err := sv.Reload()
	if err == nil {
		t.Fatal("Expected a changed socket to be refused")
	}
	if !strings.Contains(err.Error(), "socket") {
		t.Errorf("Error should name the setting, got: %v", err)
	}
}

func TestReload_NoChangeIsANoOp(t *testing.T) {
	sv, _ := newReloadServer(t, `
programs:
  app:
    command: /bin/sleep 60
    startsecs: 1
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "app", process.StateRunning, 10*time.Second)
	before := sv.process("app").GetPID()

	applied, err := sv.Reload()
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if !applied.Empty() {
		t.Errorf("Reload with no changes should report nothing, got %+v", applied)
	}
	if after := sv.process("app").GetPID(); after != before {
		t.Errorf("Reload with no changes restarted the program: pid %d -> %d", before, after)
	}
}

func TestDiffPrograms(t *testing.T) {
	old := map[string]*config.ProgramConfig{
		"keep":    {Name: "keep", Command: "/bin/true"},
		"gone":    {Name: "gone", Command: "/bin/true"},
		"altered": {Name: "altered", Command: "/bin/true"},
	}
	updated := map[string]*config.ProgramConfig{
		"keep":    {Name: "keep", Command: "/bin/true"},
		"altered": {Name: "altered", Command: "/bin/false"},
		"fresh":   {Name: "fresh", Command: "/bin/true"},
	}

	added, removed, changed := diffPrograms(old, updated)
	if len(added) != 1 || added[0] != "fresh" {
		t.Errorf("added = %v, expected [fresh]", added)
	}
	if len(removed) != 1 || removed[0] != "gone" {
		t.Errorf("removed = %v, expected [gone]", removed)
	}
	if len(changed) != 1 || changed[0] != "altered" {
		t.Errorf("changed = %v, expected [altered]", changed)
	}
}

// TestReload_ReRunsAReplacedOneOff covers a completed task being redefined: the
// completion belonged to the old definition, so the new one runs and latches on
// its own account. A dependent that is already up stays up.
func TestReload_ReRunsAReplacedOneOff(t *testing.T) {
	sv, write := newReloadServer(t, `
programs:
  migrate:
    command: /bin/sh -c 'exit 0'
    autorestart: never
    startsecs: 1
  api:
    command: /bin/sleep 60
    startsecs: 1
    autorestart: never
    depends_on:
      migrate:
        condition: completed
`)
	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "migrate", process.StateExited, 10*time.Second)
	waitForState(t, sv, "api", process.StateRunning, 10*time.Second)
	apiPID := sv.process("api").GetPID()

	write(`
programs:
  migrate:
    command: /bin/sh -c 'exit 0'
    autorestart: never
    startsecs: 2
  api:
    command: /bin/sleep 60
    startsecs: 1
    autorestart: never
    depends_on:
      migrate:
        condition: completed
`)

	applied, err := sv.Reload()
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if !slices.Contains(applied.Changed, "migrate") {
		t.Fatalf("Expected migrate to be reported as changed, got %+v", applied)
	}

	// The replacement runs on its own account rather than inheriting what the
	// definition it replaced achieved.
	waitForState(t, sv, "migrate", process.StateExited, 10*time.Second)
	if !sv.process("migrate").HasCompleted() {
		t.Error("Expected the replaced task to run again and latch")
	}

	if pid := sv.process("api").GetPID(); pid != apiPID {
		t.Errorf("Dependent was restarted by the reload: pid went from %d to %d", apiPID, pid)
	}
}
