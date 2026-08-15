package process

import (
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testReaper(t *testing.T) *childReaper {
	t.Helper()
	return reaperFor(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// spawnExit starts a child that exits with the given code, through the reaper
func spawnExit(t *testing.T, r *childReaper, code int) <-chan syscall.WaitStatus {
	t.Helper()

	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	exited, err := r.spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	return exited
}

// TestReaper_DeliversEachStatusToItsOwner is the regression test for the reason
// this exists. A reaper that calls wait4(-1) alongside os/exec steals statuses
// from it, losing exit codes intermittently; every exit has to reach the caller
// that started it, and carry the right code.
func TestReaper_DeliversEachStatusToItsOwner(t *testing.T) {
	r := testReaper(t)

	const runs = 60
	var wg sync.WaitGroup
	wrong := make(chan string, runs)

	for i := range runs {
		// A spread of codes, so a status delivered to the wrong owner is
		// visible rather than coincidentally correct.
		want := i%7 + 1
		wg.Go(func() {
			exited := spawnExit(t, r, want)
			select {
			case status := <-exited:
				if got := exitCodeOfStatus(status); got != want {
					wrong <- fmt.Sprintf("got exit %d, want %d", got, want)
				}
			case <-time.After(30 * time.Second):
				wrong <- "timed out waiting for the exit status"
			}
		})
	}

	wg.Wait()
	close(wrong)
	for msg := range wrong {
		t.Error(msg)
	}
}

// TestReaper_ReportsAKilledProcess covers the other half of exitCodeOfStatus:
// a process that was signaled never had an exit code of its own.
func TestReaper_ReportsAKilledProcess(t *testing.T) {
	r := testReaper(t)

	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	exited, err := r.spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	select {
	case status := <-exited:
		if !status.Signaled() {
			t.Errorf("Expected a signaled status, got %v", status)
		}
		if got, want := exitCodeOfStatus(status), 128+int(syscall.SIGKILL); got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for the kill to be reported")
	}
}

func TestExitErrorOf(t *testing.T) {
	// Exit status n is encoded in the high byte; a signal is the low seven bits.
	tests := []struct {
		name   string
		want   string
		status syscall.WaitStatus
	}{
		{"clean exit", "", syscall.WaitStatus(0)},
		{"exit 7", "exit status 7", syscall.WaitStatus(7 << 8)},
		{"killed", "signal: killed", syscall.WaitStatus(syscall.SIGKILL)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := exitErrorOf(tc.status)
			if tc.want == "" {
				if err != nil {
					t.Errorf("Expected no error, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Errorf("Got %v, want %s", err, tc.want)
			}
		})
	}
}
