package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/process"
)

// newTestServer builds a server over a throwaway socket and pid file. Paths go
// under /tmp because a unix socket path is limited to ~104 bytes.
func newTestServer(t *testing.T, programs map[string]*config.ProgramConfig) *Server {
	t.Helper()

	tmpDir, err := os.MkdirTemp("/tmp", "sv-rec")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	for name, prog := range programs {
		prog.Name = name
		if prog.Environment == nil {
			prog.Environment = make(map[string]string)
		}
		if prog.StdoutLogfile == "" {
			prog.StdoutLogfile = filepath.Join(tmpDir, name+".log")
		}
	}

	cfg := &config.Config{
		Supavisor: config.SupavisorConfig{
			Socket:  filepath.Join(tmpDir, "s.sock"),
			PidFile: filepath.Join(tmpDir, "sv.pid"),
		},
		Programs: programs,
	}

	sv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return sv
}

// waitForState polls until a process reaches the wanted state
func waitForState(t *testing.T, sv *Server, name string, want process.State, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if sv.process(name).GetState() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Process %s is %s after %v, expected %s", name, sv.process(name).GetState(), within, want)
}

// TestReconcile_StartsDependentOnceDependencyIsUp is the regression test for a
// dependent being stranded for good. Starting used to fail outright if a
// dependency was not running yet, and nothing ever looked again.
func TestReconcile_StartsDependentOnceDependencyIsUp(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"slowdep": {
			Command:     "/bin/sleep 60",
			Autostart:   true,
			Autorestart: config.RestartNever,
			// Slow enough that a dependent checking once would give up
			StartSecs:   3,
			MaxRestarts: 3,
		},
		"dependent": {
			Command:     "/bin/sleep 60",
			Autostart:   true,
			Autorestart: config.RestartNever,
			DependsOn:   []string{"slowdep"},
			StartSecs:   1,
			MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// The dependent must not jump the queue.
	time.Sleep(500 * time.Millisecond)
	if state := sv.process("dependent").GetState(); state != process.StateStopped {
		t.Errorf("Dependent should wait for its dependency, got %s", state)
	}

	waitForState(t, sv, "slowdep", process.StateRunning, 10*time.Second)
	waitForState(t, sv, "dependent", process.StateRunning, 10*time.Second)
}

// TestReconcile_StartsDependentWithoutBeingAskedAgain covers the property the
// old code lacked entirely: nothing re-examined a dependent once its first
// start attempt had failed, so it stayed down even after the dependency
// appeared.
func TestReconcile_StartsDependentWithoutBeingAskedAgain(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"dep": {
			Command: "/bin/sleep 60", Autostart: false,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
		"dependent": {
			Command: "/bin/sleep 60", Autostart: true,
			Autorestart: config.RestartNever, DependsOn: []string{"dep"},
			StartSecs: 1, MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// Its dependency is not set to autostart, so the dependent has to wait.
	time.Sleep(2 * time.Second)
	if state := sv.process("dependent").GetState(); state != process.StateStopped {
		t.Fatalf("Dependent should be waiting on its dependency, got %s", state)
	}

	if err := sv.StartProcess("dep"); err != nil {
		t.Fatalf("StartProcess(dep) failed: %v", err)
	}

	// Nobody asks for the dependent: the reconciler notices the dependency is
	// up and starts it.
	waitForState(t, sv, "dependent", process.StateRunning, 10*time.Second)
}

// TestReconcile_ListsEveryConfiguredProgram covers programs that have never
// been started: they used to be missing from status entirely.
func TestReconcile_ListsEveryConfiguredProgram(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"running": {
			Command: "/bin/sleep 60", Autostart: true,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
		"neverstarted": {
			Command: "/bin/sleep 60", Autostart: false,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	statuses := sv.GetStatus()
	if len(statuses) != 2 {
		t.Fatalf("Expected all 2 configured programs in status, got %d", len(statuses))
	}

	byName := make(map[string]ProcessStatusInfo, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	if got := byName["neverstarted"].State; got != process.StateStopped {
		t.Errorf("A program that has not been started should be STOPPED, got %s", got)
	}
}

// TestReconcile_DoesNotRestartAProcessThatGaveUp keeps the reconciler from
// defeating max_restarts by starting a FATAL process over and over.
func TestReconcile_DoesNotRestartAProcessThatGaveUp(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"doomed": {
			Command:     "/bin/sh -c 'exit 1'",
			Autostart:   true,
			Autorestart: config.RestartAlways,
			StartSecs:   1,
			MaxRestarts: 1,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "doomed", process.StateFatal, 15*time.Second)

	// It must stay given up rather than being picked up again by the loop.
	restarts := sv.process("doomed").GetRestartCount()
	time.Sleep(3 * time.Second)

	if state := sv.process("doomed").GetState(); state != process.StateFatal {
		t.Errorf("A process that gave up should stay FATAL, got %s", state)
	}
	if got := sv.process("doomed").GetRestartCount(); got != restarts {
		t.Errorf("Reconciler restarted a FATAL process: restart count %d -> %d", restarts, got)
	}
}

// TestReconcile_StopIsNotUndoneByTheLoop checks that a stop sticks: desired
// state, not the last thing that happened, is what the loop drives towards.
func TestReconcile_StopIsNotUndoneByTheLoop(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"svc": {
			Command: "/bin/sleep 60", Autostart: true,
			Autorestart: config.RestartAlways, StartSecs: 1, MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "svc", process.StateRunning, 10*time.Second)

	if err := sv.StopProcess("svc"); err != nil {
		t.Fatalf("StopProcess failed: %v", err)
	}

	time.Sleep(3 * time.Second)
	if state := sv.process("svc").GetState(); state != process.StateStopped {
		t.Errorf("Stopped process came back as %s", state)
	}
}

// TestReconcile_StartAfterStopBringsAProcessBack covers the round trip through
// desired state that sctl start/stop relies on.
func TestReconcile_StartAfterStopBringsAProcessBack(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"svc": {
			Command: "/bin/sleep 60", Autostart: true,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	waitForState(t, sv, "svc", process.StateRunning, 10*time.Second)
	firstPID := sv.process("svc").GetPID()

	if err := sv.StopProcess("svc"); err != nil {
		t.Fatalf("StopProcess failed: %v", err)
	}
	if err := sv.StartProcess("svc"); err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	if state := sv.process("svc").GetState(); state != process.StateRunning {
		t.Errorf("Expected RUNNING after start, got %s", state)
	}
	if pid := sv.process("svc").GetPID(); pid == firstPID {
		t.Errorf("Expected a new process, still on pid %d", pid)
	}
}

// TestReconcile_ReportsTheRootCauseOfABlockedStart checks that a start request
// fails immediately, and names the program actually holding things up, instead
// of waiting out the timeout on a dependency chain that cannot resolve.
func TestReconcile_ReportsTheRootCauseOfABlockedStart(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"db": {
			Command: "/bin/sleep 60", Autostart: false,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
		"api": {
			Command: "/bin/sleep 60", Autostart: true, DependsOn: []string{"db"},
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
		"worker": {
			Command: "/bin/sleep 60", Autostart: true, DependsOn: []string{"api"},
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	started := time.Now()
	err := sv.StartProcess("worker")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Expected starting worker to fail while db is not set to start")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Took %v to report a start that could never succeed", elapsed)
	}
	// db is two levels down, and is the program that actually needs attention.
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("Error should name the root cause db, got: %v", err)
	}
}
