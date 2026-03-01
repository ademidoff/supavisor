package supavisor

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/process"
)

func TestStopProcess_DoesNotStopDependents(t *testing.T) {
	// Use /tmp to keep socket path under 108 bytes (Unix socket limit on macOS)
	tmpDir, err := os.MkdirTemp("/tmp", "sv-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "s.sock")
	pidPath := filepath.Join(tmpDir, "supavisor.pid")
	logDir := filepath.Join(tmpDir, "logs")
	_ = os.MkdirAll(logDir, 0755)

	cfg := &config.Config{
		Supavisor: config.SupavisorConfig{
			Socket:  socketPath,
			PidFile: pidPath,
		},
		Programs: map[string]*config.ProgramConfig{
			"dependency": {
				Name:                  "dependency",
				Command:               "/bin/sleep 60",
				Autostart:             true,
				Autorestart:           config.RestartNever,
				DependsOn:             []string{},
				StartSecs:             1,
				MaxRestarts:           3,
				StdoutLogfile:         filepath.Join(logDir, "dep.log"),
				StderrLogfile:         filepath.Join(logDir, "dep.log"),
				StdoutLogfileMaxBytes: 50 * 1024 * 1024,
				StderrLogfileMaxBytes: 50 * 1024 * 1024,
				Environment:           make(map[string]string),
			},
			"dependent": {
				Name:                  "dependent",
				Command:               "/bin/sleep 60",
				Autostart:             true,
				Autorestart:           config.RestartNever,
				DependsOn:             []string{"dependency"},
				StartSecs:             1,
				MaxRestarts:           3,
				StdoutLogfile:         filepath.Join(logDir, "app.log"),
				StderrLogfile:         filepath.Join(logDir, "app.log"),
				StdoutLogfileMaxBytes: 50 * 1024 * 1024,
				StderrLogfileMaxBytes: 50 * 1024 * 1024,
				Environment:           make(map[string]string),
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sv, err := NewSupavisor(cfg, logger)
	if err != nil {
		t.Fatalf("NewSupavisor failed: %v", err)
	}

	if err := sv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sv.Stop()

	// Wait for both processes to reach RUNNING (StartSecs is 1 for each)
	time.Sleep(2500 * time.Millisecond)

	statuses := sv.GetStatus()
	if len(statuses) != 2 {
		t.Fatalf("Expected 2 processes, got %d", len(statuses))
	}

	for _, s := range statuses {
		if s.Name == "dependency" && s.State != process.StateRunning {
			t.Fatalf("Expected dependency to be running before stop, got state: %v", s.State)
		}
	}

	// Stop the dependency - dependent should NOT be stopped
	if err := sv.StopProcess("dependency"); err != nil {
		t.Fatalf("StopProcess(dependency) failed: %v", err)
	}

	// Give a moment for any async operations
	time.Sleep(200 * time.Millisecond)

	statuses = sv.GetStatus()
	for _, s := range statuses {
		if s.Name == "dependent" {
			if s.State != process.StateRunning {
				t.Errorf("Dependent should still be running after stopping dependency (dependents are not stopped). Got state: %v", s.State)
			}
			break
		}
	}
}
