package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ademidoff/go-supervisord/internal/config"
	"github.com/ademidoff/go-supervisord/internal/logrotate"
)

// Process represents a managed process
type Process struct {
	config       *config.ProgramConfig
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

	// Control channels
	stopChan    chan struct{}
	restartChan chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc

	// Callbacks
	onStateChange    func(name string, oldState, newState State)
	onDependencyStop func(name string)
}

// NewProcess creates a new process instance
func NewProcess(cfg *config.ProgramConfig) *Process {
	ctx, cancel := context.WithCancel(context.Background())
	return &Process{
		config:      cfg,
		state:       StateStopped,
		stopChan:    make(chan struct{}),
		restartChan: make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// SetStateChangeCallback sets a callback for state changes
func (p *Process) SetStateChangeCallback(fn func(name string, oldState, newState State)) {
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
	oldState := p.state
	p.state = newState
	p.stateMutex.Unlock()

	if p.onStateChange != nil && oldState != newState {
		p.onStateChange(p.config.Name, oldState, newState)
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

	p.setState(StateStarting)

	// Setup log files
	if err := p.setupLogFiles(); err != nil {
		p.setState(StateFatal)
		return fmt.Errorf("failed to setup log files: %w", err)
	}

	// Parse command
	parts := parseCommand(p.config.Command)
	if len(parts) == 0 {
		p.setState(StateFatal)
		return fmt.Errorf("invalid command: %s", p.config.Command)
	}

	// Create command
	p.cmd = exec.CommandContext(p.ctx, parts[0], parts[1:]...)

	// Set working directory
	if p.config.Directory != "" {
		p.cmd.Dir = p.config.Directory
	}

	// Set environment
	env := os.Environ()
	for k, v := range p.config.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	p.cmd.Env = env

	// Set stdout and stderr
	p.cmd.Stdout = p.stdoutFile
	p.cmd.Stderr = p.stderrFile

	// Start the process
	if err := p.cmd.Start(); err != nil {
		p.setState(StateFatal)
		return fmt.Errorf("failed to start process: %w", err)
	}

	p.pid = p.cmd.Process.Pid
	p.startTime = time.Now()
	p.lastError = nil

	// Monitor the process
	go p.monitor()

	// Wait for startsecs to determine if start was successful
	go func() {
		time.Sleep(time.Duration(p.config.StartSecs) * time.Second)
		if p.GetState() == StateStarting {
			// Check if process is still running
			if p.cmd.Process != nil {
				if err := p.cmd.Process.Signal(syscall.Signal(0)); err == nil {
					p.setState(StateRunning)
				} else {
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
		return nil
	}

	p.setState(StateStopping)

	if p.cmd != nil && p.cmd.Process != nil {
		// Try graceful shutdown first
		p.cmd.Process.Signal(os.Interrupt)

		// Wait a bit for graceful shutdown
		done := make(chan error, 1)
		go func() {
			done <- p.cmd.Wait()
		}()

		select {
		case <-done:
			// Process exited gracefully
		case <-time.After(5 * time.Second):
			// Force kill
			p.cmd.Process.Kill()
			<-done
		}
	}

	p.cancel()
	p.stopTime = time.Now()
	p.setState(StateStopped)

	// Close log files
	p.closeLogFiles()

	return nil
}

// Restart restarts the process
func (p *Process) Restart() error {
	if err := p.Stop(); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond) // Brief pause
	return p.Start()
}

// monitor monitors the process and handles restarts
func (p *Process) monitor() {
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

		if p.GetState() != StateStopping {
			p.setState(StateExited)

			// Determine if we should restart
			shouldRestart := false
			switch p.config.Autorestart {
			case config.RestartAlways:
				shouldRestart = true
			case config.RestartUnexpected:
				// Restart if exit code is non-zero
				if p.exitCode != 0 {
					shouldRestart = true
				}
			case config.RestartNever:
				shouldRestart = false
			}

			if shouldRestart {
				p.restartCount++
				if p.restartCount <= p.config.StartRetries {
					// Wait before restarting (exponential backoff)
					backoff := time.Duration(p.restartCount) * time.Second
					if backoff > 10*time.Second {
						backoff = 10 * time.Second
					}
					time.Sleep(backoff)

					if p.GetState() != StateStopping {
						p.setState(StateBackoff)
						if err := p.Start(); err != nil {
							p.setState(StateFatal)
						}
					}
				} else {
					p.setState(StateFatal)
				}
			} else {
				p.setState(StateExited)
			}
		}

	case <-p.ctx.Done():
		// Process was stopped
		return
	}
}

// setupLogFiles sets up log file rotation
func (p *Process) setupLogFiles() error {
	if p.config.StdoutLogfile != "" {
		// Ensure directory exists
		dir := getDir(p.config.StdoutLogfile)
		if dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create log directory: %w", err)
			}
		}

		file, err := os.OpenFile(p.config.StdoutLogfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open stdout log: %w", err)
		}
		p.stdoutFile = file

		p.stdoutRotator = logrotate.NewRotator(
			p.config.StdoutLogfile,
			p.config.StdoutLogfileMaxBytes,
			p.config.StdoutLogfileBackups,
			p.config.StdoutLogfileMaxAge,
		)
	}

	if p.config.StderrLogfile != "" {
		// Ensure directory exists
		dir := getDir(p.config.StderrLogfile)
		if dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create log directory: %w", err)
			}
		}

		file, err := os.OpenFile(p.config.StderrLogfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open stderr log: %w", err)
		}
		p.stderrFile = file

		p.stderrRotator = logrotate.NewRotator(
			p.config.StderrLogfile,
			p.config.StderrLogfileMaxBytes,
			p.config.StderrLogfileBackups,
			p.config.StderrLogfileMaxAge,
		)
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
			if p.stdoutRotator != nil {
				if err := p.stdoutRotator.CheckAndRotate(); err != nil {
					// Log error but continue
				}
			}
			if p.stderrRotator != nil {
				if err := p.stderrRotator.CheckAndRotate(); err != nil {
					// Log error but continue
				}
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// closeLogFiles closes log file handles
func (p *Process) closeLogFiles() {
	if p.stdoutFile != nil {
		p.stdoutFile.Close()
		p.stdoutFile = nil
	}
	if p.stderrFile != nil {
		p.stderrFile.Close()
		p.stderrFile = nil
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
