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
	err := proc.setupLogFiles(t.Context())
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
	err := proc.setupLogFiles(t.Context())
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
	err := proc.setupLogFiles(t.Context())
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
	err := proc.setupLogFiles(t.Context())
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
	err := proc.setupLogFiles(t.Context())
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
		// An unclamped shift overflows int here and yields a zero backoff,
		// which turns the restart policy into a tight loop.
		{63, 30 * time.Second},
		{64, 30 * time.Second},
		{1000, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("restartCount_%d", tt.restartCount), func(t *testing.T) {
			backoff := backoffDuration(tt.restartCount)
			if backoff != tt.expected {
				t.Errorf("restartCount %d: expected backoff %v, got %v", tt.restartCount, tt.expected, backoff)
			}
		})
	}
}

// TestRestartAfterStop guards against reusing a canceled context across runs,
// which made a process permanently unstartable once it had been stopped.
func TestRestartAfterStop(t *testing.T) {
	logDir := t.TempDir()
	cfg := &config.ProgramConfig{
		Name:          "test",
		Command:       "/bin/sleep 60",
		Autorestart:   config.RestartNever,
		StartSecs:     1,
		MaxRestarts:   3,
		StdoutLogfile: filepath.Join(logDir, "out.log"),
		Environment:   make(map[string]string),
	}

	proc := NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	if err := proc.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	if err := proc.Restart(); err != nil {
		t.Fatalf("Restart() after Stop() failed: %v", err)
	}
	if state := proc.GetState(); state == StateFatal {
		t.Errorf("Expected process to restart, got state: %v", state)
	}
}

// TestStartWithoutLogfileGivesChildDevNull guards against handing the child a
// closed descriptor, which makes every write to stdout fail with EBADF.
func TestStartWithoutLogfileGivesChildDevNull(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "wrote-ok")
	cfg := &config.ProgramConfig{
		Name: "test",
		// Touches the marker only if writing to stdout succeeded
		Command:     fmt.Sprintf("/bin/sh -c 'echo hello && touch %s'", marker),
		Autorestart: config.RestartNever,
		StartSecs:   1,
		MaxRestarts: 3,
		Environment: make(map[string]string),
	}

	proc := NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("Child could not write to stdout when no logfile is configured: %v", err)
	}
}

// TestStopCancelsPendingRestart guards against a stop being defeated by a
// restart already queued in the monitor's backoff window.
func TestStopCancelsPendingRestart(t *testing.T) {
	logDir := t.TempDir()
	cfg := &config.ProgramConfig{
		Name:          "test",
		Command:       "/bin/sh -c 'exit 3'",
		Autorestart:   config.RestartAlways,
		StartSecs:     1,
		MaxRestarts:   6,
		StdoutLogfile: filepath.Join(logDir, "out.log"),
		Environment:   make(map[string]string),
	}

	proc := NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	// Let it exit and enter the restart backoff
	time.Sleep(500 * time.Millisecond)

	if err := proc.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
	if state := proc.GetState(); state != StateStopped {
		t.Errorf("Expected STOPPED after Stop(), got: %v", state)
	}

	countAfterStop := proc.GetRestartCount()
	time.Sleep(3 * time.Second)

	if got := proc.GetRestartCount(); got != countAfterStop {
		t.Errorf("Process restarted after Stop() reported success: restart count %d -> %d", countAfterStop, got)
	}
	if state := proc.GetState(); state != StateStopped {
		t.Errorf("Expected process to stay STOPPED, got: %v", state)
	}
}
