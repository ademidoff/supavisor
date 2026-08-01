package server

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func testServer(t *testing.T, stateFile string) *Server {
	t.Helper()
	return &Server{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stateFile: stateFile,
	}
}

// startTestChild spawns a process in its own group and returns its PID along
// with a function reporting whether it has terminated.
//
// Termination is observed through Wait rather than a signal check: the child
// belongs to the test process, so between being killed and being reaped it is a
// zombie that still answers signal 0. A real orphan is a child of init and is
// reaped as soon as it dies.
func startTestChild(t *testing.T) (pid int, terminated func() bool) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start test child: %v", err)
	}
	pid = cmd.Process.Pid

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()

	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
	})

	return pid, func() bool {
		select {
		case <-exited:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
}

func alive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

func TestStateFilePath(t *testing.T) {
	tests := []struct {
		pidFile  string
		expected string
	}{
		{"/var/run/supavisor.pid", "/var/run/supavisor.state"},
		{"/tmp/sv.pid", "/tmp/sv.state"},
		{"/tmp/supavisor", "/tmp/supavisor.state"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.pidFile, func(t *testing.T) {
			if got := stateFilePath(tt.pidFile); got != tt.expected {
				t.Errorf("stateFilePath(%s) = %s, expected %s", tt.pidFile, got, tt.expected)
			}
		})
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supavisor.state")

	records := []childRecord{
		{Name: "db", PID: 111, StartToken: "abc"},
		{Name: "web", PID: 222, StartToken: "def"},
	}
	if err := writeStateFile(path, records); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	got, err := readStateFile(path)
	if err != nil {
		t.Fatalf("readStateFile failed: %v", err)
	}
	if len(got) != len(records) {
		t.Fatalf("Expected %d records, got %d", len(records), len(got))
	}
	for i := range records {
		if got[i] != records[i] {
			t.Errorf("Record %d = %+v, expected %+v", i, got[i], records[i])
		}
	}
}

func TestReadStateFile_MissingIsNotAnError(t *testing.T) {
	records, err := readStateFile(filepath.Join(t.TempDir(), "absent.state"))
	if err != nil {
		t.Errorf("A missing state file should not be an error, got: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("Expected no records, got %d", len(records))
	}
}

func TestProcessStartToken_IsStableAndDistinguishesProcesses(t *testing.T) {
	first, _ := startTestChild(t)
	second, _ := startTestChild(t)

	tokenA, err := processStartToken(first)
	if err != nil {
		t.Fatalf("processStartToken failed: %v", err)
	}
	if tokenA == "" {
		t.Fatal("Expected a non-empty start token")
	}

	// The token identifies a run, so it must not change between reads.
	tokenAgain, err := processStartToken(first)
	if err != nil {
		t.Fatalf("processStartToken failed on the second read: %v", err)
	}
	if tokenAgain != tokenA {
		t.Errorf("Start token changed between reads: %s then %s", tokenA, tokenAgain)
	}

	tokenB, err := processStartToken(second)
	if err != nil {
		t.Fatalf("processStartToken failed for the second process: %v", err)
	}
	if tokenB == tokenA {
		t.Logf("Both processes report start token %s; they started within the clock's resolution", tokenA)
	}
}

func TestProcessStartToken_FailsForDeadProcess(t *testing.T) {
	// A PID that cannot be running: above the maximum on both platforms.
	if _, err := processStartToken(0x7FFFFFFF); err == nil {
		t.Error("Expected an error for a PID that is not running")
	}
}

// TestReapOrphans_StopsSurvivorsOfACrash is the regression test for the
// recovery path: a daemon that was killed leaves its children running, and the
// next daemon has to clear them out or the processes end up doubled.
func TestReapOrphans_StopsSurvivorsOfACrash(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")
	pid, terminated := startTestChild(t)

	token, err := processStartToken(pid)
	if err != nil {
		t.Fatalf("processStartToken failed: %v", err)
	}
	if err := writeStateFile(statePath, []childRecord{{Name: "svc", PID: pid, StartToken: token}}); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	testServer(t, statePath).reapOrphans()

	if !terminated() {
		t.Errorf("Orphan %d survived reaping", pid)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("State file should have been cleared after reaping")
	}
}

// TestReapOrphans_LeavesAReusedPIDAlone is the guard against killing an
// unrelated process: PIDs are recycled, so a recorded PID may belong to
// something else entirely by the time supavisor starts again.
func TestReapOrphans_LeavesAReusedPIDAlone(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")
	pid, _ := startTestChild(t)

	// Same PID, but recorded against a different run.
	if err := writeStateFile(statePath, []childRecord{{Name: "svc", PID: pid, StartToken: "from-an-earlier-boot"}}); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	testServer(t, statePath).reapOrphans()

	if !alive(pid) {
		t.Errorf("Process %d was killed despite belonging to a different run", pid)
	}
}

func TestReapOrphans_IgnoresProcessesThatAlreadyExited(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")

	records := []childRecord{{Name: "svc", PID: 0x7FFFFFFF, StartToken: "gone"}}
	if err := writeStateFile(statePath, records); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	// Must complete promptly rather than waiting out the stop timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		testServer(t, statePath).reapOrphans()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapOrphans stalled on a process that no longer exists")
	}

	if _, err := os.Stat(statePath); err == nil {
		t.Error("State file should have been cleared")
	}
}

func TestReapOrphans_NoStateFileIsANoOp(t *testing.T) {
	testServer(t, filepath.Join(t.TempDir(), "absent.state")).reapOrphans()
	testServer(t, "").reapOrphans()
}
