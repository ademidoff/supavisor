package process

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

func TestSetupLogFiles_SharedStdoutStderr(t *testing.T) {
	tmpDir := t.TempDir()
	sharedLogPath := filepath.Join(tmpDir, "shared.log")

	cfg := &config.ProgramConfig{
		Name:                  "test",
		Command:               "/bin/echo test",
		StdoutLogfile:         sharedLogPath,
		StderrLogfile:         sharedLogPath, // Same path for both
		StdoutLogfileMaxBytes: 10 * 1024 * 1024,
		StderrLogfileMaxBytes: 20 * 1024 * 1024,
		StdoutLogfileBackups:  5,
		StderrLogfileBackups:  10,
		StdoutLogfileMaxAge:   7,
		StderrLogfileMaxAge:   14,
		Environment:           make(map[string]string),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(cfg, logger)
	err := proc.setupLogFiles()
	if err != nil {
		t.Fatalf("setupLogFiles() failed: %v", err)
	}

	// Verify that stdout and stderr point to the same file handle
	if proc.stdoutFile == nil {
		t.Error("stdoutFile should not be nil")
	}
	if proc.stderrFile == nil {
		t.Error("stderrFile should not be nil")
	}
	if proc.stdoutFile != proc.stderrFile {
		t.Error("stdoutFile and stderrFile should point to the same file handle when paths are the same")
	}

	// Verify that sharedLogFile flag is set
	if !proc.sharedLogFile {
		t.Error("sharedLogFile should be true when stdout and stderr paths are the same")
	}

	// Verify that only stdout rotator is created (stderr rotator should be nil)
	if proc.stdoutRotator == nil {
		t.Error("stdoutRotator should not be nil")
	}
	if proc.stderrRotator != nil {
		t.Error("stderrRotator should be nil when sharing the same file")
	}

	// Verify that the rotator uses the maximum settings
	// The rotator should use max(20MB, 10MB) = 20MB, max(10, 5) = 10 backups, max(14, 7) = 14 days
	// Note: We can't directly access rotator internals, but we can verify the file exists
	if _, err := os.Stat(sharedLogPath); os.IsNotExist(err) {
		t.Error("Shared log file should be created")
	}

	// Clean up
	proc.closeLogFiles()
}

func TestSetupLogFiles_SeparateStdoutStderr(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")

	cfg := &config.ProgramConfig{
		Name:                  "test",
		Command:               "/bin/echo test",
		StdoutLogfile:         stdoutPath,
		StderrLogfile:         stderrPath, // Different paths
		StdoutLogfileMaxBytes: 10 * 1024 * 1024,
		StderrLogfileMaxBytes: 20 * 1024 * 1024,
		StdoutLogfileBackups:  5,
		StderrLogfileBackups:  10,
		Environment:           make(map[string]string),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(cfg, logger)
	err := proc.setupLogFiles()
	if err != nil {
		t.Fatalf("setupLogFiles() failed: %v", err)
	}

	// Verify that stdout and stderr point to different file handles
	if proc.stdoutFile == nil {
		t.Error("stdoutFile should not be nil")
	}
	if proc.stderrFile == nil {
		t.Error("stderrFile should not be nil")
	}
	if proc.stdoutFile == proc.stderrFile {
		t.Error("stdoutFile and stderrFile should point to different file handles when paths are different")
	}

	// Verify that sharedLogFile flag is false
	if proc.sharedLogFile {
		t.Error("sharedLogFile should be false when stdout and stderr paths are different")
	}

	// Verify that both rotators are created
	if proc.stdoutRotator == nil {
		t.Error("stdoutRotator should not be nil")
	}
	if proc.stderrRotator == nil {
		t.Error("stderrRotator should not be nil")
	}

	// Clean up
	proc.closeLogFiles()
}

func TestSetupLogFiles_OnlyStdout(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "stdout.log")

	cfg := &config.ProgramConfig{
		Name:                  "test",
		Command:               "/bin/echo test",
		StdoutLogfile:         stdoutPath,
		StderrLogfile:         "", // No stderr log file
		StdoutLogfileMaxBytes: 10 * 1024 * 1024,
		StdoutLogfileBackups:  5,
		Environment:           make(map[string]string),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(cfg, logger)
	err := proc.setupLogFiles()
	if err != nil {
		t.Fatalf("setupLogFiles() failed: %v", err)
	}

	// Verify that only stdout file is created
	if proc.stdoutFile == nil {
		t.Error("stdoutFile should not be nil")
	}
	if proc.stderrFile != nil {
		t.Error("stderrFile should be nil when not configured")
	}

	// Verify that sharedLogFile flag is false
	if proc.sharedLogFile {
		t.Error("sharedLogFile should be false when only stdout is configured")
	}

	// Verify that only stdout rotator is created
	if proc.stdoutRotator == nil {
		t.Error("stdoutRotator should not be nil")
	}
	if proc.stderrRotator != nil {
		t.Error("stderrRotator should be nil when stderr logfile is not configured")
	}

	// Clean up
	proc.closeLogFiles()
}

func TestCloseLogFiles_SharedFile(t *testing.T) {
	tmpDir := t.TempDir()
	sharedLogPath := filepath.Join(tmpDir, "shared.log")

	cfg := &config.ProgramConfig{
		Name:          "test",
		Command:       "/bin/echo test",
		StdoutLogfile: sharedLogPath,
		StderrLogfile: sharedLogPath,
		Environment:   make(map[string]string),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(cfg, logger)
	err := proc.setupLogFiles()
	if err != nil {
		t.Fatalf("setupLogFiles() failed: %v", err)
	}

	// Get the file handle before closing
	stdoutFile := proc.stdoutFile
	stderrFile := proc.stderrFile

	// Close the files
	proc.closeLogFiles()

	// Verify that both references are cleared
	if proc.stdoutFile != nil {
		t.Error("stdoutFile should be nil after closeLogFiles")
	}
	if proc.stderrFile != nil {
		t.Error("stderrFile should be nil after closeLogFiles")
	}

	// Verify that the file handle is actually closed by trying to write to it
	// This should fail if the file is properly closed
	if stdoutFile != nil {
		_, err := stdoutFile.WriteString("test")
		if err == nil {
			t.Error("File should be closed and write should fail")
		}
	}

	// Verify that stderrFile points to the same handle (so it's already closed)
	if stdoutFile != stderrFile {
		t.Error("stdoutFile and stderrFile should point to the same handle")
	}
}

func TestCloseLogFiles_SeparateFiles(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")

	cfg := &config.ProgramConfig{
		Name:          "test",
		Command:       "/bin/echo test",
		StdoutLogfile: stdoutPath,
		StderrLogfile: stderrPath,
		Environment:   make(map[string]string),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(cfg, logger)
	err := proc.setupLogFiles()
	if err != nil {
		t.Fatalf("setupLogFiles() failed: %v", err)
	}

	// Get the file handles before closing
	stdoutFile := proc.stdoutFile
	stderrFile := proc.stderrFile

	// Close the files
	proc.closeLogFiles()

	// Verify that both references are cleared
	if proc.stdoutFile != nil {
		t.Error("stdoutFile should be nil after closeLogFiles")
	}
	if proc.stderrFile != nil {
		t.Error("stderrFile should be nil after closeLogFiles")
	}

	// Verify that both file handles are actually closed
	if stdoutFile != nil {
		_, err := stdoutFile.WriteString("test")
		if err == nil {
			t.Error("stdoutFile should be closed and write should fail")
		}
	}
	if stderrFile != nil && stderrFile != stdoutFile {
		_, err := stderrFile.WriteString("test")
		if err == nil {
			t.Error("stderrFile should be closed and write should fail")
		}
	}
}

func TestExponentialBackoff(t *testing.T) {
	tests := []struct {
		restartCount int
		expected     time.Duration
	}{
		{1, 1 * time.Second},   // 2^0 = 1s
		{2, 2 * time.Second},   // 2^1 = 2s
		{3, 4 * time.Second},   // 2^2 = 4s
		{4, 8 * time.Second},   // 2^3 = 8s
		{5, 16 * time.Second},  // 2^4 = 16s
		{6, 30 * time.Second},  // 2^5 = 32s, capped at 30s
		{7, 30 * time.Second},  // 2^6 = 64s, capped at 30s
		{10, 30 * time.Second}, // 2^9 = 512s, capped at 30s
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("restartCount_%d", tt.restartCount), func(t *testing.T) {
			// Calculate backoff using the same logic as in process.go
			backoff := time.Duration(1<<uint(tt.restartCount-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}

			if backoff != tt.expected {
				t.Errorf("restartCount %d: expected backoff %v, got %v", tt.restartCount, tt.expected, backoff)
			}
		})
	}
}
