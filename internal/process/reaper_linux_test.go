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
		closing := strings.LastIndex(string(raw), ")")
		if closing < 0 {
			continue
		}
		fields := strings.Fields(string(raw)[closing+1:])
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

// waitForOrphan blocks until a grandchild has been reparented onto this process
// and reports its PID, so a test can tell a reap from an empty process tree.
func waitForOrphan(t *testing.T) int {
	t.Helper()

	self := os.Getpid()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, pid := range childrenOf(t, self) {
			// The direct child is the shell, which has already exited and been
			// collected; anything left parented to us is the orphan.
			if pid != self {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("No orphan was ever reparented onto this process")
	return 0
}

// processExists reports whether a PID is still in the process table, zombie or
// otherwise. A reaped process is gone from it entirely.
func processExists(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// childrenOf lists the PIDs currently parented to ppid
func childrenOf(t *testing.T, ppid int) []int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("Failed to read /proc: %v", err)
	}

	var children []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		parent, ok := parentOf(entry.Name())
		if ok && parent == ppid {
			children = append(children, pid)
		}
	}
	return children
}

// parentOf reads the parent PID out of /proc/<pid>/stat
func parentOf(pid string) (int, bool) {
	raw, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return 0, false
	}
	// comm is parenthesised and may contain spaces, so the fields after it are
	// found from the last ')' rather than by splitting the whole line.
	closing := strings.LastIndex(string(raw), ")")
	if closing < 0 {
		return 0, false
	}
	fields := strings.Fields(string(raw)[closing+1:])
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return parent, true
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
		startErr := cmd.Start()
		if startErr != nil {
			return 0, startErr
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

	// Sampling for zero zombies proves nothing on its own: the grandchild
	// sleeps before exiting, so an early look, or a slow machine, would find
	// none simply because it has not died yet. Wait for the orphan to actually
	// arrive first, so that what follows is a real reap and not an empty tree.
	orphan := waitForOrphan(t)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(orphan) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Errorf("Orphan %d was never reaped, zombie children: %v", orphan, zombieChildren(t))
}
