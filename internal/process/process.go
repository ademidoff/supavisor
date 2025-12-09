package process

import (
	"context"
	"fmt"
	"log"
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
	// sharedLogFile indicates if stdout and stderr share the same file handle
	sharedLogFile bool

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

	log.Printf("%s: setting state to STARTING\n", p.config.Name)
	p.setState(StateStarting)

	// Setup log files
	log.Printf("%s: setting up log files\n", p.config.Name)
	if err := p.setupLogFiles(); err != nil {
		p.setState(StateFatal)
		return fmt.Errorf("failed to setup log files: %w", err)
	}

	// Parse command
	log.Printf("%s: parsing command: %s\n", p.config.Name, p.config.Command)
	parts := parseCommand(p.config.Command)
	if len(parts) == 0 {
		p.setState(StateFatal)
		return fmt.Errorf("invalid command: %s", p.config.Command)
	}

	// Create command
	log.Printf("%s: creating command: %v\n", p.config.Name, parts)
	p.cmd = exec.CommandContext(p.ctx, parts[0], parts[1:]...)

	// Set working directory
	if p.config.Directory != "" {
		log.Printf("%s: setting working directory: %s\n", p.config.Name, p.config.Directory)
		p.cmd.Dir = p.config.Directory
	}

	// Set environment
	env := os.Environ()
	if len(p.config.Environment) > 0 {
		log.Printf("%s: setting %d environment variable(s)\n", p.config.Name, len(p.config.Environment))
	}
	for k, v := range p.config.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	p.cmd.Env = env

	// Set stdout and stderr
	p.cmd.Stdout = p.stdoutFile
	p.cmd.Stderr = p.stderrFile

	// Start the process
	log.Printf("%s: executing command\n", p.config.Name)
	if err := p.cmd.Start(); err != nil {
		log.Printf("%s: failed to start: %v\n", p.config.Name, err)
		p.setState(StateFatal)
		return fmt.Errorf("failed to start process: %w", err)
	}

	p.pid = p.cmd.Process.Pid
	p.startTime = time.Now()
	p.lastError = nil
	log.Printf("%s: started with PID %d\n", p.config.Name, p.pid)

	// Monitor the process
	go p.monitor()

	// Wait for startsecs to determine if start was successful
	go func() {
		log.Printf("%s: waiting %d seconds before checking if start was successful\n", p.config.Name, p.config.StartSecs)
		time.Sleep(time.Duration(p.config.StartSecs) * time.Second)
		if p.GetState() == StateStarting {
			// Check if process is still running
			if p.cmd.Process != nil {
				if err := p.cmd.Process.Signal(syscall.Signal(0)); err == nil {
					log.Printf("%s: start successful, setting state to RUNNING\n", p.config.Name)
					p.setState(StateRunning)
				} else {
					log.Printf("%s: start check failed, setting state to BACKOFF\n", p.config.Name)
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
		log.Printf("%s: already stopped or exited\n", p.config.Name)
		return nil
	}

	log.Printf("%s: stopping process (PID: %d)\n", p.config.Name, p.pid)
	p.setState(StateStopping)

	if p.cmd != nil && p.cmd.Process != nil {
		// Try graceful shutdown first
		log.Printf("%s: sending SIGINT for graceful shutdown\n", p.config.Name)
		p.cmd.Process.Signal(os.Interrupt)

		// Wait a bit for graceful shutdown
		done := make(chan error, 1)
		go func() {
			done <- p.cmd.Wait()
		}()

		select {
		case <-done:
			// Process exited gracefully
			log.Printf("%s: exited gracefully\n", p.config.Name)
		case <-time.After(5 * time.Second):
			// Force kill
			log.Printf("%s: graceful shutdown timeout, sending SIGKILL\n", p.config.Name)
			p.cmd.Process.Kill()
			<-done
			log.Printf("%s: force killed\n", p.config.Name)
		}
	}

	p.cancel()
	p.stopTime = time.Now()
	p.setState(StateStopped)

	// Close log files
	log.Printf("%s: closing log files\n", p.config.Name)
	p.closeLogFiles()

	log.Printf("%s: stopped successfully\n", p.config.Name)
	return nil
}

// Restart restarts the process
func (p *Process) Restart() error {
	log.Printf("%s: restarting\n", p.config.Name)
	if err := p.Stop(); err != nil {
		log.Printf("%s: error during stop phase of restart: %v\n", p.config.Name, err)
		return err
	}
	log.Printf("%s: waiting 100ms before restart\n", p.config.Name)
	time.Sleep(100 * time.Millisecond) // Brief pause
	log.Printf("%s: starting after restart\n", p.config.Name)
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
			log.Printf("%s: exited with code %d\n", p.config.Name, p.exitCode)
			p.setState(StateExited)

			// Determine if we should restart
			shouldRestart := false
			switch p.config.Autorestart {
			case config.RestartAlways:
				shouldRestart = true
				log.Printf("%s: autorestart policy is 'always', will restart\n", p.config.Name)
			case config.RestartUnexpected:
				// Restart if exit code is non-zero
				if p.exitCode != 0 {
					shouldRestart = true
					log.Printf("%s: autorestart policy is 'unexpected', exit code %d is non-zero, will restart\n", p.config.Name, p.exitCode)
				} else {
					log.Printf("%s: autorestart policy is 'unexpected', exit code %d is zero, will not restart\n", p.config.Name, p.exitCode)
				}
			case config.RestartNever:
				shouldRestart = false
				log.Printf("%s: autorestart policy is 'never', will not restart\n", p.config.Name)
			}

			if shouldRestart {
				p.restartCount++
				log.Printf("%s: restart attempt %d/%d\n", p.config.Name, p.restartCount, p.config.StartRetries)
				if p.restartCount <= p.config.StartRetries {
					// Wait before restarting (exponential backoff)
					// Exponential backoff: 1s, 2s, 4s, 8s, 16s, 32s... capped at 30s
					backoff := time.Duration(1<<uint(p.restartCount-1)) * time.Second
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
					log.Printf("%s: waiting %v before restart (exponential backoff)\n", p.config.Name, backoff)
					time.Sleep(backoff)

					if p.GetState() != StateStopping {
						log.Printf("%s: attempting restart after backoff\n", p.config.Name)
						p.setState(StateBackoff)
						if err := p.Start(); err != nil {
							log.Printf("%s: restart failed: %v\n", p.config.Name, err)
							p.setState(StateFatal)
						}
					}
				} else {
					log.Printf("%s: exceeded maximum restart attempts (%d), setting state to FATAL\n", p.config.Name, p.config.StartRetries)
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
						// Log error but continue
					}
				}
			} else {
				// Check both rotators separately
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
