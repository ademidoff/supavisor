package process

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/logrotate"
)

// Process represents a managed process
type Process struct {
	config       *config.ProgramConfig
	logger       *slog.Logger
	cmd          *exec.Cmd
	state        State
	stateMutex   sync.RWMutex
	pid          int
	exitCode     int
	startTime    time.Time
	stopTime     time.Time
	restartCount int
	lastError    error

	// Log rotation
	stdoutRotator *logrotate.Rotator
	stderrRotator *logrotate.Rotator

	// File handles for logs
	stdoutFile *os.File
	stderrFile *os.File
	// sharedLogFile indicates if stdout and stderr share the same file handle
	sharedLogFile bool

	// Control channels
	stopChan    chan struct{}
	restartChan chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc

	// Synchronization for stop
	monitorDone       chan struct{}
	stoppedExternally bool
	stopMutex         sync.Mutex

	// Callbacks
	onStateChange    func(name string, prevState, newState State)
	onDependencyStop func(name string)
}

// NewProcess creates a new process instance
func NewProcess(cfg *config.ProgramConfig, logger *slog.Logger) *Process {
	ctx, cancel := context.WithCancel(context.Background())
	// Create a logger with the component set to the process name
	procLogger := logger.With("component", cfg.Name)
	return &Process{
		config:      cfg,
		logger:      procLogger,
		state:       StateStopped,
		stopChan:    make(chan struct{}),
		restartChan: make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
		monitorDone: make(chan struct{}),
	}
}

// SetStateChangeCallback sets a callback for state changes
func (p *Process) SetStateChangeCallback(fn func(name string, prevState, newState State)) {
	p.onStateChange = fn
}

// SetDependencyStopCallback sets a callback when a dependency stops
func (p *Process) SetDependencyStopCallback(fn func(name string)) {
	p.onDependencyStop = fn
}

// GetState returns the current state
func (p *Process) GetState() State {
	p.stateMutex.RLock()
	defer p.stateMutex.RUnlock()
	return p.state
}

// setState sets the state and calls the callback
func (p *Process) setState(newState State) {
	p.stateMutex.Lock()
	prevState := p.state
	p.state = newState
	p.stateMutex.Unlock()

	if p.onStateChange != nil && prevState != newState {
		p.onStateChange(p.config.Name, prevState, newState)
	}
}

// GetPID returns the process ID
func (p *Process) GetPID() int {
	return p.pid
}

// GetExitCode returns the exit code
func (p *Process) GetExitCode() int {
	return p.exitCode
}

// GetStartTime returns the start time
func (p *Process) GetStartTime() time.Time {
	return p.startTime
}

// GetRestartCount returns the number of restarts
func (p *Process) GetRestartCount() int {
	return p.restartCount
}

// Start starts the process
func (p *Process) Start() error {
	if p.GetState() == StateRunning || p.GetState() == StateStarting {
		return fmt.Errorf("process %s is already running or starting", p.config.Name)
	}

	p.logger.Info("Setting state to STARTING")
	p.setState(StateStarting)

	// Setup log files
	p.logger.Info("Setting up log files")
	if err := p.setupLogFiles(); err != nil {
		p.setState(StateFatal)
		return fmt.Errorf("failed to setup log files: %w", err)
	}

	// Parse command
	p.logger.Info("Parsing command", "command", p.config.Command)
	parts := parseCommand(p.config.Command)
	if len(parts) == 0 {
		p.setState(StateFatal)
		return fmt.Errorf("invalid command: %s", p.config.Command)
	}

	// Create command
	p.logger.Info("Creating command", "command_parts", parts)
	p.cmd = exec.CommandContext(p.ctx, parts[0], parts[1:]...)

	// Set working directory
	if p.config.Directory != "" {
		p.logger.Info("Setting working directory", "directory", p.config.Directory)
		p.cmd.Dir = p.config.Directory
	}

	// Set environment
	env := os.Environ()
	if len(p.config.Environment) > 0 {
		p.logger.Info("Setting environment variables", "count", len(p.config.Environment))
	}
	for k, v := range p.config.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	p.cmd.Env = env

	// Set stdout and stderr
	p.cmd.Stdout = p.stdoutFile
	p.cmd.Stderr = p.stderrFile

	// Start the process
	p.logger.Info("Executing command")
	if err := p.cmd.Start(); err != nil {
		p.logger.Error("Failed to start", "error", err)
		p.setState(StateFatal)
		return fmt.Errorf("failed to start process: %w", err)
	}

	p.pid = p.cmd.Process.Pid
	p.startTime = time.Now()
	p.lastError = nil
	p.logger.Info("Started process", "pid", p.pid)

	// Reset monitor synchronization
	p.monitorDone = make(chan struct{})
	p.stopMutex.Lock()
	p.stoppedExternally = false
	p.stopMutex.Unlock()

	// Monitor the process
	go p.monitor()

	// Wait for startsecs to determine if start was successful
	go func() {
		p.logger.Info("Waiting before checking start success", "seconds", p.config.StartSecs)
		time.Sleep(time.Duration(p.config.StartSecs) * time.Second)
		if p.GetState() == StateStarting {
			// Check if process is still running
			if p.cmd.Process != nil {
				if err := p.cmd.Process.Signal(syscall.Signal(0)); err == nil {
					p.logger.Info("Start successful, setting state to RUNNING")
					p.setState(StateRunning)
				} else {
					p.logger.Info("Start check failed, setting state to BACKOFF")
					p.setState(StateBackoff)
				}
			}
		}
	}()

	return nil
}

// Stop stops the process
func (p *Process) Stop() error {
	if p.GetState() == StateStopped || p.GetState() == StateExited {
		p.logger.Info("Already stopped or exited")
		return nil
	}

	p.logger.Info("Stopping process", "pid", p.pid)

	// Mark that this stop was initiated externally (by supervisor or user command)
	p.stopMutex.Lock()
	p.stoppedExternally = true
	p.stopMutex.Unlock()

	p.setState(StateStopping)

	if p.cmd != nil && p.cmd.Process != nil {
		// Try graceful shutdown first
		p.logger.Info("Sending SIGINT for graceful shutdown")
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			p.logger.Warn("Failed to send SIGINT", "error", err)
		}

		// Wait for graceful shutdown with timeout
		select {
		case <-p.monitorDone:
			// Process exited gracefully, monitor has finished
			p.logger.Info("Process exited gracefully")
		case <-time.After(5 * time.Second):
			// Force kill
			p.logger.Info("Graceful shutdown timeout, sending SIGKILL")
			if err := p.cmd.Process.Kill(); err != nil {
				p.logger.Warn("Failed to send SIGKILL", "error", err)
			}
			// Wait for monitor to finish after kill
			<-p.monitorDone
			p.logger.Info("Force killed")
		}
	}

	// Cancel context to clean up any remaining goroutines
	p.cancel()

	// Close log files
	p.logger.Info("Closing process log files")
	p.closeLogFiles()

	p.logger.Info("Process stopped successfully")
	return nil
}

// Restart restarts the process
func (p *Process) Restart() error {
	p.logger.Info("Restarting")
	if err := p.Stop(); err != nil {
		p.logger.Error("Error during stop phase of restart", "error", err)
		return err
	}
	p.logger.Info("Waiting 100ms before restart")
	time.Sleep(100 * time.Millisecond)
	p.logger.Info("Starting after restart")
	return p.Start()
}

// monitor monitors the process and handles restarts
func (p *Process) monitor() {
	defer close(p.monitorDone)

	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case err := <-done:
		// Get exit code
		if p.cmd.ProcessState != nil {
			p.exitCode = p.cmd.ProcessState.ExitCode()
		} else if err != nil {
			// Try to extract exit code from error
			if exitError, ok := err.(*exec.ExitError); ok {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
					p.exitCode = status.ExitStatus()
				} else {
					p.exitCode = -1
				}
			} else {
				p.exitCode = -1
			}
		} else {
			p.exitCode = 0
		}
		p.stopTime = time.Now()
		p.lastError = err

		currentState := p.GetState()

		// Check if stop was initiated externally (by supervisor or user command)
		p.stopMutex.Lock()
		stoppedExternally := p.stoppedExternally
		p.stopMutex.Unlock()

		if currentState == StateStopping {
			// Process was being stopped
			if stoppedExternally {
				// Stopped externally by supervisor or user command
				p.logger.Info("Process stopped externally", "exit_code", p.exitCode)
				p.setState(StateStopped)
			} else {
				// Process exited on its own while we were trying to stop it
				p.logger.Info("Process exited during stop", "exit_code", p.exitCode)
				p.setState(StateExited)
			}
		} else if stoppedExternally {
			// Process was stopped externally but exited before we could send the signal
			p.logger.Info("Process stopped externally", "exit_code", p.exitCode)
			p.setState(StateStopped)
		} else {
			// Process exited on its own
			p.logger.Info("Process exited", "exit_code", p.exitCode)
			p.setState(StateExited)

			// Determine if we should restart
			shouldRestart := false
			switch p.config.Autorestart {
			case config.RestartAlways:
				shouldRestart = true
				p.logger.Debug("Autorestart policy is 'always', will restart")
			case config.RestartUnexpected:
				// Restart if exit code is non-zero
				if p.exitCode != 0 {
					shouldRestart = true
					p.logger.Debug("Autorestart policy is 'unexpected', exit code is non-zero, will restart", "exit_code", p.exitCode)
				} else {
					p.logger.Debug("Autorestart policy is 'unexpected', exit code is zero, will not restart", "exit_code", p.exitCode)
				}
			case config.RestartNever:
				shouldRestart = false
				p.logger.Debug("Autorestart policy is 'never', will not restart")
			}

			if shouldRestart {
				p.restartCount++
				p.logger.Info("Restart attempt", "attempt", p.restartCount, "max_retries", p.config.StartRetries)
				if p.restartCount <= p.config.StartRetries {
					// Wait before restarting (exponential backoff)
					// Exponential backoff: 1s, 2s, 4s, 8s, 16s, 32s... capped at 30s
					backoff := min(time.Duration(1<<uint(p.restartCount-1))*time.Second, 30*time.Second) //nolint:gosec
					p.logger.Info("Waiting before restart", "backoff", backoff)
					time.Sleep(backoff)

					if p.GetState() != StateStopping {
						p.logger.Info("Attempting restart after backoff")
						p.setState(StateBackoff)
						if err := p.Start(); err != nil {
							p.logger.Error("Restart failed", "error", err)
							p.setState(StateFatal)
						}
					}
				} else {
					p.logger.Error("Exceeded maximum restart attempts, setting state to FATAL", "max_retries", p.config.StartRetries)
					p.setState(StateFatal)
				}
			} else {
				p.setState(StateExited)
			}
		}

	case <-p.ctx.Done():
		// Context was cancelled, process might still be running
		// This shouldn't normally happen as Stop() waits for monitor to complete
		p.logger.Info("Monitor context cancelled")
		return
	}
}

// setupLogFiles sets up log file rotation
func (p *Process) setupLogFiles() error {
	// Check if stdout and stderr point to the same file
	stdoutPath := p.config.StdoutLogfile
	stderrPath := p.config.StderrLogfile
	p.sharedLogFile = stdoutPath != "" && stderrPath != "" && stdoutPath == stderrPath

	if p.sharedLogFile {
		// Both stdout and stderr use the same file
		// Ensure directory exists
		dir := getDir(stdoutPath)
		if dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create log directory: %w", err)
			}
		}

		file, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open shared log file: %w", err)
		}
		p.stdoutFile = file
		p.stderrFile = file // Use the same file handle for both

		// Use the maximum of the two maxbytes settings for rotation
		maxBytes := p.config.StdoutLogfileMaxBytes
		if p.config.StderrLogfileMaxBytes > maxBytes {
			maxBytes = p.config.StderrLogfileMaxBytes
		}
		// Use the maximum of the two backups settings
		backups := p.config.StdoutLogfileBackups
		if p.config.StderrLogfileBackups > backups {
			backups = p.config.StderrLogfileBackups
		}
		// Use the maximum of the two maxage settings
		maxAge := p.config.StdoutLogfileMaxAge
		if p.config.StderrLogfileMaxAge > maxAge {
			maxAge = p.config.StderrLogfileMaxAge
		}

		p.stdoutRotator = logrotate.NewRotator(
			stdoutPath,
			maxBytes,
			backups,
			maxAge,
		)
		// Don't create a separate stderr rotator
		p.stderrRotator = nil
	} else {
		// Separate files for stdout and stderr
		if stdoutPath != "" {
			// Ensure directory exists
			dir := getDir(stdoutPath)
			if dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					return fmt.Errorf("failed to create log directory: %w", err)
				}
			}

			file, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				return fmt.Errorf("failed to open stdout log: %w", err)
			}
			p.stdoutFile = file

			p.stdoutRotator = logrotate.NewRotator(
				stdoutPath,
				p.config.StdoutLogfileMaxBytes,
				p.config.StdoutLogfileBackups,
				p.config.StdoutLogfileMaxAge,
			)
		}

		if stderrPath != "" {
			// Ensure directory exists
			dir := getDir(stderrPath)
			if dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					return fmt.Errorf("failed to create log directory: %w", err)
				}
			}

			file, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				return fmt.Errorf("failed to open stderr log: %w", err)
			}
			p.stderrFile = file

			p.stderrRotator = logrotate.NewRotator(
				stderrPath,
				p.config.StderrLogfileMaxBytes,
				p.config.StderrLogfileBackups,
				p.config.StderrLogfileMaxAge,
			)
		}
	}

	// Start log rotation monitoring
	go p.monitorLogRotation()

	return nil
}

// monitorLogRotation periodically checks and rotates logs
func (p *Process) monitorLogRotation() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if p.sharedLogFile {
				// Only check stdout rotator since both streams share the same file
				if p.stdoutRotator != nil {
					if err := p.stdoutRotator.CheckAndRotate(); err != nil {
						p.logger.Error("Failed to rotate shared log", "error", err)
					}
				}
			} else {
				// Check both rotators separately
				if p.stdoutRotator != nil {
					if err := p.stdoutRotator.CheckAndRotate(); err != nil {
						p.logger.Error("Failed to rotate stdout log", "error", err)
					}
				}
				if p.stderrRotator != nil {
					if err := p.stderrRotator.CheckAndRotate(); err != nil {
						p.logger.Error("Failed to rotate stderr log", "error", err)
					}
				}
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// closeLogFiles closes log file handles
func (p *Process) closeLogFiles() {
	if p.sharedLogFile {
		// Only close once since both stdout and stderr share the same file handle
		if p.stdoutFile != nil {
			p.stdoutFile.Close()
			p.stdoutFile = nil
			p.stderrFile = nil // Clear the reference but don't close again
		}
	} else {
		// Close both files separately
		if p.stdoutFile != nil {
			p.stdoutFile.Close()
			p.stdoutFile = nil
		}
		if p.stderrFile != nil && p.stderrFile != p.stdoutFile {
			p.stderrFile.Close()
			p.stderrFile = nil
		}
	}
}

// parseCommand parses a command string into parts
func parseCommand(cmd string) []string {
	parts := []string{}
	current := ""
	inQuotes := false
	quoteChar := byte(0)

	for i := 0; i < len(cmd); i++ {
		char := cmd[i]

		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			} else {
				current += string(char)
			}
		} else if char == ' ' && !inQuotes {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(char)
		}
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}

func getDir(path string) string {
	idx := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	return path[:idx]
}

// Signal sends a signal to the process
func (p *Process) Signal(sig os.Signal) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return fmt.Errorf("process is not running")
	}
	return p.cmd.Process.Signal(sig)
}
