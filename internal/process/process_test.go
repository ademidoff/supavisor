package process

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

func logCfg(stdoutPath, stderrPath string) *config.ProgramConfig {
	return &config.ProgramConfig{
		Name:                  "test",
		Command:               "/bin/echo test",
		StdoutLogfile:         stdoutPath,
		StderrLogfile:         stderrPath,
		StdoutLogfileMaxBytes: 10 * 1024 * 1024,
		StderrLogfileMaxBytes: 20 * 1024 * 1024,
		StdoutLogfileBackups:  5,
		StderrLogfileBackups:  10,
		Environment:           make(map[string]string),
	}
}

func TestStartLogging_SharedStdoutStderr(t *testing.T) {
	sharedLogPath := filepath.Join(t.TempDir(), "shared.log")
	proc := NewProcess(logCfg(sharedLogPath, sharedLogPath), slog.New(slog.NewTextHandler(io.Discard, nil)))

	stdout, stderr, err := proc.startLogging()
	if err != nil {
		t.Fatalf("startLogging() failed: %v", err)
	}
	t.Cleanup(proc.stopLogging)

	if stdout == nil || stderr == nil {
		t.Fatal("Both stdout and stderr should be captured")
	}
	if stdout != stderr {
		t.Error("Both streams should share one pipe when they share a log file")
	}
	if len(proc.streams) != 1 {
		t.Errorf("Expected 1 capture stream, got %d", len(proc.streams))
	}
	if _, err := os.Stat(sharedLogPath); err != nil {
		t.Errorf("Shared log file should be created: %v", err)
	}
}

func TestStartLogging_SeparateStdoutStderr(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")
	proc := NewProcess(logCfg(stdoutPath, stderrPath), slog.New(slog.NewTextHandler(io.Discard, nil)))

	stdout, stderr, err := proc.startLogging()
	if err != nil {
		t.Fatalf("startLogging() failed: %v", err)
	}
	t.Cleanup(proc.stopLogging)

	if stdout == nil || stderr == nil {
		t.Fatal("Both stdout and stderr should be captured")
	}
	if stdout == stderr {
		t.Error("Separate log files should get separate pipes")
	}
	if len(proc.streams) != 2 {
		t.Errorf("Expected 2 capture streams, got %d", len(proc.streams))
	}
}

func TestStartLogging_OnlyStdout(t *testing.T) {
	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	proc := NewProcess(logCfg(stdoutPath, ""), slog.New(slog.NewTextHandler(io.Discard, nil)))

	stdout, stderr, err := proc.startLogging()
	if err != nil {
		t.Fatalf("startLogging() failed: %v", err)
	}
	t.Cleanup(proc.stopLogging)

	if stdout == nil {
		t.Error("stdout should be captured")
	}
	if stderr != nil {
		t.Error("stderr should be discarded when no stderr logfile is configured")
	}
	if len(proc.streams) != 1 {
		t.Errorf("Expected 1 capture stream, got %d", len(proc.streams))
	}
}

func TestStopLogging_IsIdempotent(t *testing.T) {
	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	proc := NewProcess(logCfg(stdoutPath, ""), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, _, err := proc.startLogging(); err != nil {
		t.Fatalf("startLogging() failed: %v", err)
	}

	proc.stopLogging()
	if len(proc.streams) != 0 {
		t.Errorf("Expected streams to be released, got %d", len(proc.streams))
	}
	proc.stopLogging()
}

// TestCapturedOutputIsWritten checks the whole path: child writes to the pipe,
// the drain goroutine copies it, and the tail is flushed before the exit is
// recorded.
func TestCapturedOutputIsWritten(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "out.log")
	cfg := logCfg(logPath, logPath)
	cfg.Command = "/bin/sh -c 'echo first; echo second >&2'"
	cfg.Autorestart = config.RestartNever
	cfg.StartSecs = 1

	proc := NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	time.Sleep(500 * time.Millisecond)

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log: %v", err)
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("Log should contain %s, got: %s", want, content)
		}
	}
}

// TestRotationKeepsCapturingOutput is the regression test for rename-based
// rotation: the child used to hold the log descriptor, so after the first
// rotation it kept writing to the renamed file and the current log stayed
// empty forever while the backup grew without bound.
func TestRotationKeepsCapturingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "out.log")

	cfg := logCfg(logPath, logPath)
	cfg.Command = "/bin/sh -c 'i=0; while [ $i -lt 400 ]; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaline$i; i=$((i+1)); done; sleep 30'"
	cfg.Autorestart = config.RestartNever
	cfg.StartSecs = 1
	cfg.StdoutLogfileMaxBytes = 2048
	cfg.StderrLogfileMaxBytes = 2048
	cfg.StdoutLogfileBackups = 3
	cfg.StderrLogfileBackups = 3

	proc := NewProcess(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	time.Sleep(2 * time.Second)

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("Expected the log to have rotated: %v", err)
	}

	// The size limit must still hold after rotation, and the current log must
	// be the file that is actually receiving output.
	entries, err := filepath.Glob(logPath + "*")
	if err != nil {
		t.Fatalf("Glob failed: %v", err)
	}
	for _, entry := range entries {
		info, statErr := os.Stat(entry)
		if statErr != nil {
			t.Fatalf("Failed to stat %s: %v", entry, statErr)
		}
		// Draining line by line means a file can only exceed the limit by a
		// partial line, not by a whole read buffer.
		if info.Size() > 2048+256 {
			t.Errorf("%s is %d bytes, past the 2048 byte limit", entry, info.Size())
		}
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Failed to stat current log: %v", err)
	}
	if info.Size() == 0 {
		t.Error("Current log is empty after rotation: output is going somewhere else")
	}

	// Backups beyond the configured count must be pruned.
	if _, err := os.Stat(logPath + ".4"); err == nil {
		t.Error("Backup .4 exists but only 3 backups are configured")
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
