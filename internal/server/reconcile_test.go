package server

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/process"
)

// newTestServer builds a server over a throwaway socket and pid file. Paths go
// under /tmp because a unix socket path is limited to ~104 bytes.
func newTestServer(t *testing.T, programs map[string]*config.ProgramConfig) *Server {
	t.Helper()
	return newTestServerWithLogger(t, programs, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newTestServerWithLogger is newTestServer for a test that reads the log
func newTestServerWithLogger(t *testing.T, programs map[string]*config.ProgramConfig, logger *slog.Logger) *Server {
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

	sv, err := New(cfg, logger)
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
			DependsOn:   []config.Dependency{{Name: "slowdep"}},
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
			Autorestart: config.RestartNever, DependsOn: []config.Dependency{{Name: "dep"}},
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

// TestReconcile_WaitsForADependencyToBeHealthy covers the gap RUNNING leaves:
// the dependency's process is up well before it can serve, and a dependent that
// starts in that window fails to connect and exits.
func TestReconcile_WaitsForADependencyToBeHealthy(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")

	sv := newTestServer(t, map[string]*config.ProgramConfig{
		// Alive immediately, able to serve two seconds later
		"db": {
			Command:     "/bin/sh -c 'sleep 2; touch " + ready + "; sleep 60'",
			Autostart:   true,
			Autorestart: config.RestartNever,
			StartSecs:   1,
			MaxRestarts: 1,
			HealthCheck: &config.HealthCheck{
				Exec:     "/bin/sh -c 'test -f " + ready + "'",
				Interval: 100 * time.Millisecond,
				Timeout:  2 * time.Second,
				Retries:  1,
			},
		},
		"api": {
			Command:     "/bin/sleep 60",
			Autostart:   true,
			Autorestart: config.RestartNever,
			DependsOn:   []config.Dependency{{Name: "db", Condition: config.ConditionHealthy}},
			StartSecs:   1,
			MaxRestarts: 1,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// db is RUNNING a second before it is ready, which is exactly the window a
	// dependent must not start in.
	waitForState(t, sv, "db", process.StateRunning, 10*time.Second)
	if state := sv.process("api").GetState(); state != process.StateStopped {
		t.Errorf("Dependent started while its dependency was running but not healthy, state is %s", state)
	}

	waitForState(t, sv, "api", process.StateRunning, 15*time.Second)

	byName := make(map[string]ProcessStatusInfo)
	for _, status := range sv.GetStatus() {
		byName[status.Name] = status
	}
	if got := byName["db"].Health; got != process.HealthHealthy {
		t.Errorf("Expected db to report %s, got %s", process.HealthHealthy, got)
	}
	if got := byName["api"].Health; got != process.HealthNone {
		t.Errorf("A program without a health check should report %s, got %s", process.HealthNone, got)
	}
}

// TestReconcile_StartedConditionIgnoresHealth keeps the existing meaning of
// depends_on: a dependency that declares a health check does not start gating
// dependents that only asked for it to be running.
func TestReconcile_StartedConditionIgnoresHealth(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"db": {
			Command:     "/bin/sleep 60",
			Autostart:   true,
			Autorestart: config.RestartNever,
			StartSecs:   1,
			MaxRestarts: 1,
			HealthCheck: &config.HealthCheck{
				Exec:     "/bin/sh -c 'exit 1'",
				Interval: 100 * time.Millisecond,
				Timeout:  2 * time.Second,
				Retries:  1,
			},
		},
		"api": {
			Command:     "/bin/sleep 60",
			Autostart:   true,
			Autorestart: config.RestartNever,
			DependsOn:   []config.Dependency{{Name: "db"}},
			StartSecs:   1,
			MaxRestarts: 1,
		},
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// db never passes its check, and api must come up regardless.
	waitForState(t, sv, "api", process.StateRunning, 15*time.Second)
}

// TestReconcile_ReportsABlockedStartOncePerReason covers the log of a program
// that is waiting: the reconciler looks every second, so repeating the same
// reason would bury everything else, and saying nothing left a program that
// never starts with no explanation at all.
func TestReconcile_ReportsABlockedStartOncePerReason(t *testing.T) {
	var log lockedBuffer
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo}))

	sv := newTestServerWithLogger(t, map[string]*config.ProgramConfig{
		"dep": {
			Command: "/bin/sleep 60", Autostart: false,
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 1,
		},
		"dependent": {
			Command: "/bin/sleep 60", Autostart: true,
			Autorestart: config.RestartNever, DependsOn: []config.Dependency{{Name: "dep"}},
			StartSecs: 1, MaxRestarts: 1,
		},
	}, logger)

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = sv.Stop() })

	// Several reconcile passes over a situation that has not changed
	time.Sleep(3 * time.Second)

	reasons := blockedReasons(log.String())
	if len(reasons) != 1 {
		t.Fatalf("Expected one report while nothing changed, got %d: %v", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], "dep") {
		t.Errorf("Expected the report to name the dependency, got %s", reasons[0])
	}

	// Bringing the dependency up moves the dependent through further reasons on
	// its way to starting; none of them may repeat.
	if err := sv.StartProcess("dep"); err != nil {
		t.Fatalf("StartProcess(dep) failed: %v", err)
	}
	waitForState(t, sv, "dependent", process.StateRunning, 10*time.Second)

	reasons = blockedReasons(log.String())
	seen := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		if seen[reason] {
			t.Errorf("Reason reported more than once: %s", reason)
		}
		seen[reason] = true
	}
}

// lockedBuffer collects log output written from the reconcile loop while the
// test reads it
type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockedReasons returns the reason of every "Not starting yet" line logged
func blockedReasons(log string) []string {
	reasons := make([]string, 0)

	for line := range strings.SplitSeq(log, "\n") {
		if !strings.Contains(line, "Not starting yet") {
			continue
		}
		_, reason, found := strings.Cut(line, "reason=")
		if found {
			reasons = append(reasons, reason)
		}
	}
	return reasons
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
			Command: "/bin/sleep 60", Autostart: true, DependsOn: []config.Dependency{{Name: "db"}},
			Autorestart: config.RestartNever, StartSecs: 1, MaxRestarts: 3,
		},
		"worker": {
			Command: "/bin/sleep 60", Autostart: true, DependsOn: []config.Dependency{{Name: "api"}},
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

// TestStop_IsParallelWithinATier guards the shutdown budget. Stopping serially
// cost stopwaitsecs per unresponsive process, so a handful of them exceeded
// systemd's TimeoutStopSec and the daemon was killed with its processes still
// running.
func TestStop_IsParallelWithinATier(t *testing.T) {
	ignoresTerm := "/bin/sh -c 'trap \"\" TERM INT; while true; do sleep 0.1; done'"
	stubborn := func() *config.ProgramConfig {
		return &config.ProgramConfig{
			Command: ignoresTerm, Autostart: true, Autorestart: config.RestartNever,
			StartSecs: 1, StopWaitSecs: 2, StopSignal: syscall.SIGTERM, MaxRestarts: 1,
		}
	}
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"a": stubborn(), "b": stubborn(), "c": stubborn(),
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		waitForState(t, sv, name, process.StateRunning, 10*time.Second)
	}

	started := time.Now()
	if err := sv.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	elapsed := time.Since(started)

	// Serially this would be 3 x stopwaitsecs; in parallel it is one.
	if elapsed > 5*time.Second {
		t.Errorf("Shutdown took %v, which is serial rather than parallel", elapsed)
	}
}

// TestStop_WorksInwardsFromDependents checks shutdown order: stopping a
// database before the programs using it is the wrong way round, and map
// iteration order used to decide.
func TestStop_WorksInwardsFromDependents(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "sv-order")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	orderFile := filepath.Join(tmpDir, "order")

	// Each program records its name when it is asked to stop
	recordOnTerm := func(name string) string {
		return "/bin/sh -c 'trap \"echo " + name + " >> " + orderFile + "; exit 0\" TERM; while true; do sleep 0.1; done'"
	}

	recorder := func(name string, dependsOn []config.Dependency) *config.ProgramConfig {
		return &config.ProgramConfig{
			Command: recordOnTerm(name), Autostart: true, Autorestart: config.RestartNever,
			DependsOn: dependsOn, StartSecs: 1, StopWaitSecs: 5,
			StopSignal: syscall.SIGTERM, MaxRestarts: 1,
		}
	}
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"db":     recorder("db", nil),
		"api":    recorder("api", []config.Dependency{{Name: "db"}}),
		"worker": recorder("worker", []config.Dependency{{Name: "api"}}),
	})

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	for _, name := range []string{"db", "api", "worker"} {
		waitForState(t, sv, name, process.StateRunning, 20*time.Second)
	}

	if err := sv.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	recorded, err := os.ReadFile(orderFile)
	if err != nil {
		t.Fatalf("Failed to read stop order: %v", err)
	}
	got := strings.Fields(string(recorded))
	want := []string{"worker", "api", "db"}
	if len(got) != len(want) {
		t.Fatalf("Expected all 3 programs to record a stop, got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Stopped in order %v, expected %v", got, want)
		}
	}
}

// TestPriorityOrdersReconciliation checks that priority is honored among
// programs that are all ready at once. It used to be parsed and never used.
func TestPriorityOrdersReconciliation(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"last":   {Command: "/bin/sleep 60", Autostart: true, Priority: 900, StartSecs: 1, MaxRestarts: 1},
		"first":  {Command: "/bin/sleep 60", Autostart: true, Priority: 10, StartSecs: 1, MaxRestarts: 1},
		"middle": {Command: "/bin/sleep 60", Autostart: true, Priority: 500, StartSecs: 1, MaxRestarts: 1},
	})

	got := sv.programNames()
	want := []string{"first", "middle", "last"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Reconcile order %v, expected %v", got, want)
		}
	}
}

// TestProgramNamesIsStableWithoutPriorities keeps the order deterministic when
// nothing distinguishes the programs.
func TestProgramNamesIsStableWithoutPriorities(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"c": {Command: "/bin/sleep 60", Priority: 999, StartSecs: 1, MaxRestarts: 1},
		"a": {Command: "/bin/sleep 60", Priority: 999, StartSecs: 1, MaxRestarts: 1},
		"b": {Command: "/bin/sleep 60", Priority: 999, StartSecs: 1, MaxRestarts: 1},
	})

	got := sv.programNames()
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Order %v, expected %v", got, want)
		}
	}
}
