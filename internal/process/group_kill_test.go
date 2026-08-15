package process

import (
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

// groupTestConfig is a program definition that is never started; these tests
// only exercise the group cleanup, which takes a PID rather than a run.
func groupTestConfig(t *testing.T) *config.ProgramConfig {
	t.Helper()

	return &config.ProgramConfig{
		Name:          "group-cleanup",
		Command:       "/bin/true",
		StdoutLogfile: filepath.Join(t.TempDir(), "out.log"),
		Environment:   make(map[string]string),
	}
}

// TestKillLingeringGroup_LeavesAReusedPIDAlone covers the hazard that the group
// id outlives the leader it was named after.
//
// By the time the group is cleaned up the leader has been reaped, so its PID is
// free. If something else has taken that number, the group id is no longer ours
// and signaling it would kill a stranger's whole tree. Here a live process
// stands in for the reused PID: its group must survive.
func TestKillLingeringGroup_LeavesAReusedPIDAlone(t *testing.T) {
	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r := reaperFor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	exited, err := r.spawn(func() (int, error) {
		startErr := cmd.Start()
		if startErr != nil {
			return 0, startErr
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	bystander := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-bystander, syscall.SIGKILL)
		<-exited
	})

	// Setpgid made it the leader of its own group, so its PID doubles as a
	// group id: exactly the shape a recycled leader PID would have.
	p := NewProcess(groupTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.killLingeringGroup(bystander)

	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(bystander, syscall.Signal(0)) != nil {
		t.Fatal("A live process holding the PID was killed as if it were our leftover process group")
	}
}

// TestKillLingeringGroup_ClearsRealLeftovers is the other half: when the PID is
// genuinely gone, whatever remains in the group is ours and has to be cleaned
// up, or a wrapper script's real workload is leaked.
func TestKillLingeringGroup_ClearsRealLeftovers(t *testing.T) {
	// The wrapper backgrounds a grandchild and exits, leaving the group behind.
	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r := reaperFor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	exited, err := r.spawn(func() (int, error) {
		startErr := cmd.Start()
		if startErr != nil {
			return 0, startErr
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	leader := cmd.Process.Pid
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for the wrapper to exit")
	}

	// The leader is reaped and its PID is gone, so the group is unambiguously
	// the remains of our run.
	if syscall.Kill(leader, syscall.Signal(0)) == nil {
		t.Skip("The leader PID was reused already, which this test cannot control")
	}

	p := NewProcess(groupTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.killLingeringGroup(leader)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-leader, syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("The lingering process group was not cleaned up")
}

func TestAwaitClose(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !awaitClose(closed, time.Second) {
		t.Error("Expected an already-closed channel to be reported as closed")
	}

	// The case that matters: a monitor that never reports, which used to hang
	// Stop and the whole shutdown behind it.
	never := make(chan struct{})
	start := time.Now()
	if awaitClose(never, 200*time.Millisecond) {
		t.Error("Expected a channel that never closes to time out")
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Errorf("Returned after %s, before the timeout elapsed", waited)
	}
}
