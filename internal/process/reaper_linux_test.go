//go:build linux

package process

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// prSetChildSubreaper makes this process the reaper for orphaned descendants,
// which is the position PID 1 occupies in a container. It lets the orphan path
// be tested without actually being PID 1.
const prSetChildSubreaper = 36

// zombieChildren counts our own children that have exited and not been reaped
func zombieChildren(t *testing.T) []int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("Failed to read /proc: %v", err)
	}

	var zombies []int
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// comm is parenthesised and may contain spaces, so the fields after it
		// are found from the last ')' rather than by splitting the whole line.
		close := strings.LastIndex(string(raw), ")")
		if close < 0 {
			continue
		}
		fields := strings.Fields(string(raw)[close+1:])
		if len(fields) < 2 {
			continue
		}
		state, ppid := fields[0], fields[1]
		if state == "Z" && ppid == strconv.Itoa(self) {
			zombies = append(zombies, pid)
		}
	}
	return zombies
}

// TestReaper_ReapsOrphansItDidNotStart is the container case: a supervised
// program spawns a child and exits, the child is reparented to us, and when it
// exits there is nobody but us to wait for it. Without reaping it stays a
// zombie forever.
func TestReaper_ReapsOrphansItDidNotStart(t *testing.T) {
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Skipf("Cannot become a child subreaper: %v", errno)
	}
	t.Cleanup(func() {
		_, _, _ = syscall.Syscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0)
	})

	r := testReaper(t)

	// The direct child exits at once, orphaning a grandchild that exits shortly
	// afterwards and lands on us.
	//nolint:noctx // the reaper owns the wait; a context-bound command would fight it
	cmd := exec.Command("/bin/sh", "-c", "(sleep 1; exit 3) & exit 0")
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

	select {
	case status := <-exited:
		if got := exitCodeOfStatus(status); got != 0 {
			t.Errorf("The direct child exited %d, want 0", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Timed out waiting for the direct child")
	}

	// The grandchild is still sleeping; once it exits it is ours to reap.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(zombieChildren(t)) == 0 {
			time.Sleep(2 * time.Second)
			if zombies := zombieChildren(t); len(zombies) == 0 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Errorf("Orphan left unreaped: zombie children %v", zombieChildren(t))
}
