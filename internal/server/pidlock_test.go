package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquirePIDLock_WritesOwnPID(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "supavisor.pid")

	lock, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDLock failed: %v", err)
	}
	defer lock.Release()

	content, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("Failed to read pid file: %v", err)
	}
	if got, want := strings.TrimSpace(string(content)), fmt.Sprintf("%d", os.Getpid()); got != want {
		t.Errorf("Pid file holds %s, expected %s", got, want)
	}
}

func TestAcquirePIDLock_RejectsSecondHolder(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "supavisor.pid")

	lock, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("First acquirePIDLock failed: %v", err)
	}
	defer lock.Release()

	_, err = acquirePIDLock(pidPath)
	if err == nil {
		t.Fatal("Second acquirePIDLock should have failed while the first holds the lock")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("Expected an 'already running' error naming the holder, got: %v", err)
	}
}

// TestAcquirePIDLock_StaleFileIsNotAnObstacle is the regression test for a
// crashed daemon leaving a PID file behind: startup used to refuse with "found
// stale PID file ... remove it manually", so a supervisor under a restart policy
// could never recover on its own.
func TestAcquirePIDLock_StaleFileIsNotAnObstacle(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "supavisor.pid")

	// A PID file left behind by a daemon that died without cleaning up. The
	// kernel dropped its flock, so the file alone must not block startup.
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("Failed to seed stale pid file: %v", err)
	}

	lock, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDLock should succeed over a stale file, got: %v", err)
	}
	_ = lock.Release()
}

func TestPIDLock_ReleaseRemovesFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "supavisor.pid")

	lock, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDLock failed: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if _, err := os.Stat(pidPath); err == nil {
		t.Error("Release should have removed the pid file")
	}

	// The lock must be reusable straight away.
	next, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDLock after Release failed: %v", err)
	}
	_ = next.Release()
}

// TestPIDLock_ReleaseKeepsAReplacedFile is the regression test for a shutting
// down daemon deleting the next daemon's PID file. Shutdown can take seconds
// per process, so it overlaps with the incoming daemon's startup.
func TestPIDLock_ReleaseKeepsAReplacedFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "supavisor.pid")

	outgoing, err := acquirePIDLock(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDLock failed: %v", err)
	}

	// The incoming daemon replaces the path with a file of its own while the
	// outgoing one is still shutting down.
	if err := os.Remove(pidPath); err != nil {
		t.Fatalf("Failed to remove pid file: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("4242\n"), 0o644); err != nil {
		t.Fatalf("Failed to write replacement pid file: %v", err)
	}

	if err := outgoing.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	content, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("Outgoing daemon deleted the replacement pid file: %v", err)
	}
	if strings.TrimSpace(string(content)) != "4242" {
		t.Errorf("Replacement pid file was modified, holds: %s", content)
	}
}

func TestRemoveIfSame(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "target")

	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}

	// A path that no longer exists is not an error.
	missing := filepath.Join(tmpDir, "missing")
	if err := removeIfSame(missing, info); err != nil {
		t.Errorf("removeIfSame on a missing path should be a no-op, got: %v", err)
	}

	if err := removeIfSame(path, info); err != nil {
		t.Fatalf("removeIfSame failed: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("removeIfSame should have removed our own file")
	}
}
