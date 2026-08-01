package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidLock is an exclusive lock held on the daemon's PID file for as long as the
// daemon runs.
//
// The lock, not the file contents, is what keeps two daemons from running at
// once. Reading the file and checking whether that PID is alive is a race, and
// it cannot tell a live daemon from a crashed one, which is why a crash used to
// leave behind a file that an operator had to remove by hand before supavisor
// would start again. A flock is released by the kernel when the holder dies, so
// a crashed daemon leaves nothing to clean up.
type pidLock struct {
	file *os.File
	path string
}

// maxPIDFileBytes bounds how much of a pid file is read when reporting which
// daemon holds the lock.
const maxPIDFileBytes = 32

// acquirePIDLock locks path and records the current PID in it
func acquirePIDLock(path string) (*pidLock, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create pid file directory %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open pid file %s: %w", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner := readPID(file)
		_ = file.Close()
		if owner > 0 {
			return nil, fmt.Errorf("supavisor is already running (PID: %d)", owner)
		}
		return nil, fmt.Errorf("pid file %s is held by another process: %w", path, err)
	}

	if err := writePID(file, os.Getpid()); err != nil {
		_ = file.Close()
		return nil, err
	}

	return &pidLock{file: file, path: path}, nil
}

// Release removes the PID file and drops the lock
func (l *pidLock) Release() error {
	// Unlink only our own file. A shutdown can take several seconds and overlap
	// with the next daemon's startup, and removing by path alone would delete
	// the incoming daemon's PID file, leaving it running and unfindable.
	var rmErr error
	if info, err := l.file.Stat(); err == nil {
		rmErr = removeIfSame(l.path, info)
	}

	// Closing the descriptor releases the flock.
	if err := l.file.Close(); err != nil {
		return err
	}
	return rmErr
}

func writePID(file *os.File, pid int) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("failed to truncate pid file: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to rewind pid file: %w", err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", pid); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}
	return file.Sync()
}

// readPID reads the PID recorded in an already-open pid file
func readPID(file *os.File) int {
	if _, err := file.Seek(0, 0); err != nil {
		return 0
	}

	buf := make([]byte, maxPIDFileBytes)
	n, err := file.Read(buf)
	if n == 0 || (err != nil && n == 0) {
		return 0
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}

// removeIfSame removes path only if it still refers to the same file we created.
// Anything else means a newer daemon has already replaced it and it is not ours
// to delete.
func removeIfSame(path string, want os.FileInfo) error {
	got, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if !os.SameFile(want, got) {
		return nil
	}
	return os.Remove(path)
}
