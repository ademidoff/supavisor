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

	boot, err := bootID()
	if err != nil {
		t.Fatalf("bootID failed: %v", err)
	}
	return &Server{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stateFile: stateFile,
		bootID:    boot,
	}
}

// startTestChild spawns a process in its own group and returns its PID along
// with a function reporting whether it has terminated.
//
// Termination is observed by signal, not by Wait. The reaper started in
// TestMain owns every wait4 in this binary, so waiting here as well would race
// it for the status. It also removes the reason this used to wait: a killed
// child is collected by the reaper rather than lingering as a zombie that still
// answers signal 0.
func startTestChild(t *testing.T) (pid int, terminated func() bool) {
	t.Helper()

	//nolint:noctx // the reaper owns the wait, so a context-bound command would fight it
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := cmd.Start()
	if err != nil {
		t.Fatalf("Failed to start test child: %v", err)
	}
	pid = cmd.Process.Pid

	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		// Nothing waited on it, so the handle is ours to give back.
		_ = cmd.Process.Release()
	})

	return pid, func() bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !alive(pid) {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
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
	if err := writeStateFile(path, "boot-1", records); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	got, err := readStateFile(path)
	if err != nil {
		t.Fatalf("readStateFile failed: %v", err)
	}
	if got.BootID != "boot-1" {
		t.Errorf("BootID = %s, expected boot-1", got.BootID)
	}
	if len(got.Children) != len(records) {
		t.Fatalf("Expected %d records, got %d", len(records), len(got.Children))
	}
	for i := range records {
		if got.Children[i] != records[i] {
			t.Errorf("Record %d = %+v, expected %+v", i, got.Children[i], records[i])
		}
	}
}

func TestReadStateFile_MissingIsNotAnError(t *testing.T) {
	state, err := readStateFile(filepath.Join(t.TempDir(), "absent.state"))
	if err != nil {
		t.Errorf("A missing state file should not be an error, got: %v", err)
	}
	if state != nil {
		t.Errorf("Expected no state, got %+v", state)
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
	sv := testServer(t, statePath)
	if err := writeStateFile(statePath, sv.bootID, []childRecord{{Name: "svc", PID: pid, StartToken: token}}); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	sv.reapOrphans()

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

	// Same PID and the same boot, but recorded against a different run.
	sv := testServer(t, statePath)
	if err := writeStateFile(statePath, sv.bootID, []childRecord{{Name: "svc", PID: pid, StartToken: "a-different-run"}}); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	sv.reapOrphans()

	if !alive(pid) {
		t.Errorf("Process %d was killed despite belonging to a different run", pid)
	}
}

func TestReapOrphans_IgnoresProcessesThatAlreadyExited(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")

	sv := testServer(t, statePath)
	records := []childRecord{{Name: "svc", PID: 0x7FFFFFFF, StartToken: "gone"}}
	if err := writeStateFile(statePath, sv.bootID, records); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	// Must complete promptly rather than waiting out the stop timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sv.reapOrphans()
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

func TestBootID_IsStable(t *testing.T) {
	first, err := bootID()
	if err != nil {
		t.Fatalf("bootID failed: %v", err)
	}
	if first == "" {
		t.Fatal("Expected a non-empty boot id")
	}

	second, err := bootID()
	if err != nil {
		t.Fatalf("bootID failed on the second read: %v", err)
	}
	if second != first {
		t.Errorf("Boot id changed between reads: %s then %s", first, second)
	}
}

// TestReapOrphans_LeavesProcessesFromAnEarlierBootAlone is the guard against
// killing an unrelated process after a reboot.
//
// A Linux start token is ticks since boot, so it repeats every boot: a recorded
// PID could match a process that merely started at the same offset of a later
// one. That is only reachable when the state file outlives the reboot, which
// needs a pidfile on persistent storage rather than the default under /run.
func TestReapOrphans_LeavesProcessesFromAnEarlierBootAlone(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")
	pid, _ := startTestChild(t)

	token, err := processStartToken(pid)
	if err != nil {
		t.Fatalf("processStartToken failed: %v", err)
	}

	// Everything matches except the boot the record belongs to.
	records := []childRecord{{Name: "svc", PID: pid, StartToken: token}}
	if err := writeStateFile(statePath, "a-previous-boot", records); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	testServer(t, statePath).reapOrphans()

	if !alive(pid) {
		t.Errorf("Process %d was killed on a record from an earlier boot", pid)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("A state file from an earlier boot should have been discarded")
	}
}

// TestReapOrphans_SkipsWhenTheBootCannotBeIdentified keeps the failure
// direction safe: unable to verify means leave it alone, not kill it.
func TestReapOrphans_SkipsWhenTheBootCannotBeIdentified(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supavisor.state")
	pid, _ := startTestChild(t)

	token, err := processStartToken(pid)
	if err != nil {
		t.Fatalf("processStartToken failed: %v", err)
	}
	if err := writeStateFile(statePath, "", []childRecord{{Name: "svc", PID: pid, StartToken: token}}); err != nil {
		t.Fatalf("writeStateFile failed: %v", err)
	}

	sv := testServer(t, statePath)
	sv.bootID = ""
	sv.reapOrphans()

	if !alive(pid) {
		t.Errorf("Process %d was killed although the boot could not be identified", pid)
	}
}
