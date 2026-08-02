package process

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

// healthProc builds a process with a health check over a throwaway log file
func healthProc(t *testing.T, command string, check *config.HealthCheck) *Process {
	t.Helper()

	cfg := &config.ProgramConfig{
		Name:                  "health",
		Command:               command,
		Environment:           make(map[string]string),
		Autorestart:           config.RestartNever,
		StopSignal:            syscall.SIGTERM,
		StopWaitSecs:          2,
		StdoutLogfile:         filepath.Join(t.TempDir(), "out.log"),
		StdoutLogfileMaxBytes: 1024 * 1024,
		StdoutLogfileBackups:  1,
		HealthCheck:           check,
	}
	return NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fastCheck probes often enough to keep the tests short
func fastCheck(check *config.HealthCheck) *config.HealthCheck {
	check.Interval = 50 * time.Millisecond
	check.Timeout = 2 * time.Second
	if check.Retries == 0 {
		check.Retries = 1
	}
	return check
}

func waitForHealth(t *testing.T, proc *Process, want Health, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if proc.GetHealth() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Health is %s after %v, expected %s", proc.GetHealth(), within, want)
}

// TestHealth_NoCheckReportsNothing keeps a program without a health check out of
// the health machinery entirely.
func TestHealth_NoCheckReportsNothing(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", nil)

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	time.Sleep(300 * time.Millisecond)
	if health := proc.GetHealth(); health != HealthNone {
		t.Errorf("A program without a health check should report %s, got %s", HealthNone, health)
	}
}

func TestHealth_ExecProbeReportsHealthy(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{Exec: "/bin/sh -c 'exit 0'"}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthHealthy, 3*time.Second)
}

// TestHealth_ExecProbeRunsWithTheProgramEnvironment covers a probe that needs
// the same settings the program was given, such as a database client reading
// PGPORT.
func TestHealth_ExecProbeRunsWithTheProgramEnvironment(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{
		Exec: "/bin/sh -c 'test \"$PROBE_TOKEN\" = ready'",
	}))
	proc.config.Environment["PROBE_TOKEN"] = "ready"

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthHealthy, 3*time.Second)
}

// TestHealth_FailingProbeLeavesTheProgramRunning is the scope boundary of this
// feature: supavisor reports what it observes and gates dependents, it does not
// take a working process down because a probe disagrees.
func TestHealth_FailingProbeLeavesTheProgramRunning(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{
		Exec:    "/bin/sh -c 'exit 1'",
		Retries: 2,
	}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthUnhealthy, 3*time.Second)

	if state := proc.GetState(); state != StateRunning && state != StateStarting {
		t.Errorf("A failing probe must not stop the program, state is %s", state)
	}
	if proc.GetRestartCount() != 0 {
		t.Errorf("A failing probe must not count as a restart, got %d", proc.GetRestartCount())
	}
}

// TestHealth_StartPeriodHoldsOffUnhealthy covers the window a program gets to
// initialize in: a database rejecting connections during bootstrap is expected,
// not a fault to report.
func TestHealth_StartPeriodHoldsOffUnhealthy(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{
		Exec:        "/bin/sh -c 'exit 1'",
		StartPeriod: time.Second,
	}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	// Several probes have failed by now, and none of them counts.
	time.Sleep(400 * time.Millisecond)
	if health := proc.GetHealth(); health != HealthStarting {
		t.Errorf("Failures inside start_period should leave the program %s, got %s", HealthStarting, health)
	}

	waitForHealth(t, proc, HealthUnhealthy, 3*time.Second)
}

func TestHealth_TCPProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{TCP: listener.Addr().String()}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthHealthy, 3*time.Second)
}

func TestHealth_HTTPProbe(t *testing.T) {
	tests := []struct {
		name   string
		want   Health
		status int
	}{
		{name: "ready", want: HealthHealthy, status: http.StatusOK},
		{name: "not ready", want: HealthUnhealthy, status: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(server.Close)

			proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{HTTP: server.URL + "/readyz"}))

			if err := proc.Start(); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			t.Cleanup(func() { _ = proc.Stop() })

			waitForHealth(t, proc, tt.want, 3*time.Second)
		})
	}
}

// TestHealth_ProbingStopsWhenTheProcessExits keeps a checker from outliving the
// process it was checking, where it would go on reporting on whatever else
// answers at that address.
func TestHealth_ProbingStopsWhenTheProcessExits(t *testing.T) {
	proc := healthProc(t, "/bin/sh -c 'sleep 0.5'", fastCheck(&config.HealthCheck{Exec: "/bin/sh -c 'exit 0'"}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthHealthy, 3*time.Second)
	waitForHealth(t, proc, HealthNone, 5*time.Second)
}

// TestHealth_ProbingStopsWhenTheProcessExitsImmediately is the same property for
// a process that is already gone by the time the checker is set up. The checker
// has to be in place before the exit is handled, or nothing tears it down and it
// goes on probing for the lifetime of the daemon.
func TestHealth_ProbingStopsWhenTheProcessExitsImmediately(t *testing.T) {
	proc := healthProc(t, "/bin/sh -c 'exit 0'", fastCheck(&config.HealthCheck{Exec: "/bin/sh -c 'exit 0'"}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	waitForHealth(t, proc, HealthNone, 3*time.Second)

	// Nothing may revive it: a leaked checker would report on a dead program.
	time.Sleep(300 * time.Millisecond)
	if health := proc.GetHealth(); health != HealthNone {
		t.Errorf("A checker outlived the process it was checking, health is %s", health)
	}
}

// TestHealth_StopClearsHealth covers the same for a deliberate stop
func TestHealth_StopClearsHealth(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{Exec: "/bin/sh -c 'exit 0'"}))

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	waitForHealth(t, proc, HealthHealthy, 3*time.Second)

	if err := proc.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if health := proc.GetHealth(); health != HealthNone {
		t.Errorf("A stopped program should report %s, got %s", HealthNone, health)
	}
}

func TestHealth_ChangeCallback(t *testing.T) {
	proc := healthProc(t, "/bin/sleep 30", fastCheck(&config.HealthCheck{Exec: "/bin/sh -c 'exit 0'"}))

	seen := make(chan Health, 8)
	proc.SetHealthChangeCallback(func(_ string, _, health Health) {
		select {
		case seen <- health:
		default:
		}
	})

	if err := proc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	// STARTING first, then HEALTHY once the probe passes
	for _, want := range []Health{HealthStarting, HealthHealthy} {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("Expected a %s notification, got %s", want, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("No %s notification arrived", want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{name: "empty", out: "   \n", want: ""},
		{name: "single line", out: "connection refused\n", want: "connection refused"},
		{name: "first line only", out: "connection refused\nretrying\n", want: "connection refused"},
		{name: "truncated", out: strings.Repeat("x", maxProbeOutputBytes+10), want: strings.Repeat("x", maxProbeOutputBytes) + "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine([]byte(tt.out)); got != tt.want {
				t.Errorf("firstLine() = %s, want %s", got, tt.want)
			}
		})
	}
}
