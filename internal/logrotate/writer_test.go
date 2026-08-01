package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriter_RotatesAtMaxBytes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")

	w, err := NewWriter(logPath, 1024, 3, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	line := []byte(strings.Repeat("A", 200) + "\n")
	for range 10 {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("Expected a rotated backup: %v", err)
	}

	// Every file, current and rotated, must respect the limit.
	entries, err := filepath.Glob(logPath + "*")
	if err != nil {
		t.Fatalf("Glob failed: %v", err)
	}
	for _, entry := range entries {
		info, statErr := os.Stat(entry)
		if statErr != nil {
			t.Fatalf("Failed to stat %s: %v", entry, statErr)
		}
		if info.Size() > 1024 {
			t.Errorf("%s is %d bytes, past the 1024 byte limit", entry, info.Size())
		}
	}
}

func TestWriter_KeepsWritingToCurrentFileAfterRotation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")

	w, err := NewWriter(logPath, 512, 3, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte(strings.Repeat("A", 600) + "\n")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if _, err := w.Write([]byte("after-rotation\n")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read current log: %v", err)
	}
	if !strings.Contains(string(content), "after-rotation") {
		t.Errorf("Current log should hold post-rotation writes, got: %s", content)
	}
}

func TestWriter_WriteLargerThanMaxBytes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")

	w, err := NewWriter(logPath, 100, 3, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	// A single write past the limit must still land rather than rotate forever.
	big := []byte(strings.Repeat("A", 500))
	n, err := w.Write(big)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(big) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(big), n)
	}
}

func TestWriter_PrunesBackupsBeyondCount(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	for _, num := range []int{1, 2, 3, 4, 5} {
		if err := os.WriteFile(backupPath(logPath, num), []byte("old"), 0o644); err != nil {
			t.Fatalf("Failed to seed backup: %v", err)
		}
	}

	w, err := NewWriter(logPath, 1024, 2, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	for _, num := range []int{1, 2} {
		if _, statErr := os.Stat(backupPath(logPath, num)); statErr != nil {
			t.Errorf("Backup .%d should have been kept: %v", num, statErr)
		}
	}
	for _, num := range []int{3, 4, 5} {
		if _, statErr := os.Stat(backupPath(logPath, num)); statErr == nil {
			t.Errorf("Backup .%d should have been pruned", num)
		}
	}
}

func TestWriter_PrunesExpiredBackups(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	fresh := backupPath(logPath, 1)
	stale := backupPath(logPath, 2)
	for _, path := range []string{fresh, stale} {
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatalf("Failed to seed backup: %v", err)
		}
	}

	old := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Failed to age backup: %v", err)
	}

	w, err := NewWriter(logPath, 1024, 5, 7)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("Backup inside maxage should have been kept: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("Backup older than maxage should have been pruned")
	}
}

func TestWriter_AppendsToExistingFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")

	if err := os.WriteFile(logPath, []byte("existing\n"), 0o644); err != nil {
		t.Fatalf("Failed to seed log: %v", err)
	}

	w, err := NewWriter(logPath, 1024*1024, 3, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("appended\n")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log: %v", err)
	}
	if !strings.Contains(string(content), "existing") || !strings.Contains(string(content), "appended") {
		t.Errorf("Expected both existing and appended content, got: %s", content)
	}
}

func TestWriter_WriteAfterClose(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")

	w, err := NewWriter(logPath, 1024, 3, 0)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close should be idempotent, got: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("Write after Close should fail")
	}
}

func TestBackupPath(t *testing.T) {
	tests := []struct {
		expected string
		num      int
	}{
		{"/var/log/test.log.1", 1},
		{"/var/log/test.log.2", 2},
		{"/var/log/test.log.5", 5},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if result := backupPath("/var/log/test.log", tt.num); result != tt.expected {
				t.Errorf("backupPath(%d) = %s, expected %s", tt.num, result, tt.expected)
			}
		})
	}
}

func TestBackupNumber(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		ok       bool
	}{
		{"1", 1, true},
		{"10", 10, true},
		{"123", 123, true},
		{"0", 0, false},
		{"abc", 0, false},
		{"1a", 0, false},
		{"", 0, false},
		{"-1", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, ok := backupNumber(tt.input)
			if result != tt.expected || ok != tt.ok {
				t.Errorf("backupNumber(%s) = (%d, %v), expected (%d, %v)", tt.input, result, ok, tt.expected, tt.ok)
			}
		})
	}
}
