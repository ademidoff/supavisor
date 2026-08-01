// Package logrotate writes captured process output to a log file, rotating it
// by size and pruning old backups.
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Writer is an io.WriteCloser that appends to a log file and rotates it once it
// would grow past maxBytes.
//
// The writer owns the file descriptor. Rotation closes, renames and reopens,
// which only works because the process producing the output writes to a pipe
// rather than to this file: a child holding its own descriptor would keep
// writing to the renamed inode and the new file would stay empty forever.
type Writer struct {
	file     *os.File
	path     string
	maxBytes int64
	size     int64
	backups  int
	maxAge   int
	mu       sync.Mutex
}

// NewWriter opens path for appending, creating its directory if needed
func NewWriter(path string, maxBytes int64, backups, maxAge int) (*Writer, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create log directory %s: %w", dir, err)
		}
	}

	file, err := openAppend(path)
	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to stat log file %s: %w", path, err)
	}

	w := &Writer{
		file:     file,
		path:     path,
		maxBytes: maxBytes,
		size:     info.Size(),
		backups:  backups,
		maxAge:   maxAge,
	}
	w.prune()

	return w, nil
}

// Path returns the log file being written
func (w *Writer) Path() string {
	return w.path
}

// Write appends p, rotating first if it would take the file past maxBytes
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}

	// Only rotate when there is content to preserve, so that a single write
	// larger than maxBytes still lands somewhere instead of rotating forever.
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		// A rotation that fails must not stop the process from logging, so the
		// error is only fatal when it left us without an open file.
		if err := w.rotate(); err != nil && w.file == nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close releases the log file
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}

	err := w.file.Close()
	w.file = nil
	return err
}

// rotate shifts the backup chain and reopens an empty log. Must hold mu.
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("failed to close log before rotation: %w", err)
	}
	w.file = nil

	shiftErr := w.shiftBackups()

	// Reopen whatever happened above: losing the descriptor would silently end
	// this process's logging, which is worse than a failed rotation.
	file, err := openAppend(w.path)
	if err != nil {
		return err
	}
	w.file = file
	w.size = 0
	if info, statErr := file.Stat(); statErr == nil {
		w.size = info.Size()
	}

	w.prune()
	return shiftErr
}

// shiftBackups renames log.N to log.N+1 and the current log to log.1
func (w *Writer) shiftBackups() error {
	if w.backups <= 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to discard current log: %w", err)
		}
		return nil
	}

	for i := w.backups - 1; i >= 1; i-- {
		oldPath := backupPath(w.path, i)
		if _, err := os.Stat(oldPath); err != nil {
			continue
		}
		if err := os.Rename(oldPath, backupPath(w.path, i+1)); err != nil {
			return fmt.Errorf("failed to rotate backup %d: %w", i, err)
		}
	}

	if err := os.Rename(w.path, backupPath(w.path, 1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to rotate current log: %w", err)
	}
	return nil
}

// prune removes numbered backups beyond the configured count and, when maxAge
// is set, those older than the limit. Must hold mu.
func (w *Writer) prune() {
	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var cutoff time.Time
	if w.maxAge > 0 {
		cutoff = time.Now().AddDate(0, 0, -w.maxAge)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), base+".") {
			continue
		}
		num, ok := backupNumber(strings.TrimPrefix(entry.Name(), base+"."))
		if !ok {
			continue
		}

		expired := false
		if w.maxAge > 0 {
			info, infoErr := entry.Info()
			expired = infoErr == nil && info.ModTime().Before(cutoff)
		}

		if num > w.backups || expired {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func openAppend(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", path, err)
	}
	return file, nil
}

func backupPath(path string, num int) string {
	return fmt.Sprintf("%s.%d", path, num)
}

// backupNumber parses the numeric suffix of a rotated log file
func backupNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}

	num := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		num = num*10 + int(r-'0')
	}
	if num == 0 {
		return 0, false
	}
	return num, true
}
