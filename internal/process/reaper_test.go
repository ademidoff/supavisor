package process

import (
	"errors"
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

// spawnExit starts a child that exits with the given code, through the reaper.
//
// It returns the error rather than calling t.Fatalf, because it is called from
// goroutines other than the test's own, where Fatalf is not allowed.
func spawnExit(r *childReaper, code int) (<-chan syscall.WaitStatus, error) {
	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	return r.spawn(func() (int, error) {
		startErr := cmd.Start()
		if startErr != nil {
			return 0, startErr
		}
		return cmd.Process.Pid, nil
	})
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
			exited, err := spawnExit(r, want)
			if err != nil {
				wrong <- fmt.Sprintf("spawn failed: %v", err)
				return
			}
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
		startErr := cmd.Start()
		if startErr != nil {
			return 0, startErr
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
		// -1 for a signaled process, matching os.ProcessState.ExitCode and
		// therefore the exit_code field the status API has always reported.
		if got := exitCodeOfStatus(status); got != exitCodeSignalled {
			t.Errorf("exit code = %d, want %d", got, exitCodeSignalled)
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

// TestReaper_AStuckStartDoesNotBlockReaping is the regression test for holding
// a lock across a fork.
//
// A start that wedges in exec — a binary on a hung mount, say — used to stall
// every other program's start and all exit delivery with it, because reaping
// had to wait for the fork to finish. Nothing is held across the fork now, so
// a stuck start is only stuck for itself.
func TestReaper_AStuckStartDoesNotBlockReaping(t *testing.T) {
	r := testReaper(t)

	forking := make(chan struct{})
	release := make(chan struct{})
	stuck := make(chan struct{})

	go func() {
		defer close(stuck)
		_, _ = r.spawn(func() (int, error) {
			close(forking)
			// A fork that never returns.
			<-release
			return 0, errors.New("abandoned")
		})
	}()

	<-forking
	defer func() {
		close(release)
		<-stuck
	}()

	// A real child exits while that start is still wedged. Its status has to
	// arrive regardless.
	exited, err := spawnExit(r, 5)
	if err != nil {
		t.Fatalf("spawn failed while another start was stuck: %v", err)
	}

	select {
	case status := <-exited:
		if got := exitCodeOfStatus(status); got != 5 {
			t.Errorf("exit code = %d, want 5", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("A start stuck in fork blocked exit delivery for everything else")
	}
}
