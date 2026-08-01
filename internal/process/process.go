package process

import (
	"context"
	"errors"
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

const (
	maxBackoffSeconds       = 30
	gracefulShutdownTimeout = 5 * time.Second
	logRotationInterval     = 5 * time.Second
	restartWaitInterval     = 100 * time.Millisecond

	// maxBackoffShift caps the exponent used for restart backoff. Without it
	// 1<<(restartCount-1) overflows int once max_restarts grows past ~63 and
	// yields a zero backoff, turning the restart policy into a tight loop.
	maxBackoffShift = 5

	// healthyUptime is how long a process must stay up for the run to count as
	// successful. Reaching it resets the consecutive-restart counter so that
	// max_restarts bounds crash loops rather than restarts over the whole
	// lifetime of the daemon.
	healthyUptime = 60 * time.Second
)

// Process represents a managed process
type Process struct {
	config *config.ProgramConfig
	logger *slog.Logger

	// Callbacks, set once before the process is started
	onStateChange    func(name string, prevState, newState State)
	onDependencyStop func(name string)

	cmd           *exec.Cmd
	cancel        context.CancelFunc
	monitorDone   chan struct{}
	stdoutFile    *os.File
	stderrFile    *os.File
	stdoutRotator *logrotate.Rotator
	stderrRotator *logrotate.Rotator
	lastError     error
	startTime     time.Time
	stopTime      time.Time
	state         State

	// mu guards all mutable run state, from cmd down to stoppedExternally.
	// The monitor goroutine writes it while the IPC path reads it.
	mu           sync.RWMutex
	pid          int
	exitCode     int
	restartCount int
	// sharedLogFile indicates if stdout and stderr share the same file handle
	sharedLogFile     bool
	stoppedExternally bool
}

// NewProcess creates a new process instance
func NewProcess(cfg *config.ProgramConfig, logger *slog.Logger) *Process {
	return &Process{
		config: cfg,
		logger: logger.With("component", "process", "process", cfg.Name),
		state:  StateStopped,
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// setState sets the state and calls the callback
func (p *Process) setState(newState State) {
	p.mu.Lock()
	prevState := p.state
	p.state = newState
	p.mu.Unlock()

	if p.onStateChange != nil && prevState != newState {
		p.onStateChange(p.config.Name, prevState, newState)
	}
}

// compareAndSetState transitions from one state to another only if the process
// is still in the expected state, and reports whether it did.
func (p *Process) compareAndSetState(from, to State) bool {
	p.mu.Lock()
	if p.state != from {
		p.mu.Unlock()
		return false
	}
	p.state = to
	p.mu.Unlock()

	if p.onStateChange != nil && from != to {
		p.onStateChange(p.config.Name, from, to)
	}
	return true
}

// GetPID returns the process ID
func (p *Process) GetPID() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pid
}

// GetExitCode returns the exit code
func (p *Process) GetExitCode() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exitCode
}

// GetStartTime returns the start time
func (p *Process) GetStartTime() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.startTime
}

// GetRestartCount returns the number of restarts
func (p *Process) GetRestartCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.restartCount
}

// Start starts the process
func (p *Process) Start() error {
	p.mu.Lock()
	if p.state == StateRunning || p.state == StateStarting {
		p.mu.Unlock()
		return fmt.Errorf("process %s is already running or starting", p.config.Name)
	}
	// Every run gets a fresh context. Stop() cancels the previous one and
	// exec.CommandContext refuses to start against a canceled context, so
	// reusing it would make a process unstartable after its first stop.
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	p.logger.Info("Setting state to STARTING")
	p.setState(StateStarting)

	p.logger.Info("Setting up log files")
	if err := p.setupLogFiles(ctx); err != nil {
		cancel()
		p.setState(StateFatal)
		return fmt.Errorf("failed to setup log files: %w", err)
	}

	cmd, err := p.buildCommand(ctx)
	if err != nil {
		cancel()
		p.setState(StateFatal)
		return err
	}

	p.logger.Info("Executing command", "command", cmd.String())
	if err := cmd.Start(); err != nil {
		p.logger.Error("Failed to start", "error", err)
		cancel()
		p.setState(StateFatal)
		return fmt.Errorf("failed to start process: %w", err)
	}

	monitorDone := make(chan struct{})

	p.mu.Lock()
	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.startTime = time.Now()
	p.lastError = nil
	p.monitorDone = monitorDone
	p.stoppedExternally = false
	pid := p.pid
	p.mu.Unlock()

	p.logger.Info("Started process", "pid", pid)

	go p.monitor(ctx, cmd, monitorDone)
	go p.waitForStartSuccess(ctx, cmd)

	return nil
}

// buildCommand assembles the exec.Cmd for a single run
func (p *Process) buildCommand(ctx context.Context) (*exec.Cmd, error) {
	p.logger.Debug("Parsing command", "command", p.config.Command)
	parts := parseCommand(p.config.Command)
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid command: %s", p.config.Command)
	}

	p.logger.Debug("Creating command", "command_parts", parts)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...) //nolint:gosec

	if p.config.Directory != "" {
		p.logger.Info("Setting working directory", "directory", p.config.Directory)
		cmd.Dir = p.config.Directory
	}

	env := os.Environ()
	if len(p.config.Environment) > 0 {
		p.logger.Info("Setting environment variables", "count", len(p.config.Environment))
	}
	for k, v := range p.config.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	// Leave Stdout/Stderr nil when no log file is configured: assigning a nil
	// *os.File would hand the child a closed descriptor instead of /dev/null.
	p.mu.RLock()
	stdoutFile, stderrFile := p.stdoutFile, p.stderrFile
	p.mu.RUnlock()
	if stdoutFile != nil {
		cmd.Stdout = stdoutFile
	}
	if stderrFile != nil {
		cmd.Stderr = stderrFile
	}

	return cmd, nil
}

// waitForStartSuccess promotes the process to RUNNING once it has stayed alive
// for startsecs.
func (p *Process) waitForStartSuccess(ctx context.Context, cmd *exec.Cmd) {
	p.logger.Info("Waiting before checking start success", "seconds", p.config.StartSecs)

	select {
	case <-time.After(time.Duration(p.config.StartSecs) * time.Second):
	case <-ctx.Done():
		return
	}

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		if p.compareAndSetState(StateStarting, StateBackoff) {
			p.logger.Info("Start check failed, setting state to BACKOFF")
		}
		return
	}
	if p.compareAndSetState(StateStarting, StateRunning) {
		p.logger.Info("Start successful, setting state to RUNNING")
	}
}

// Stop stops the process
func (p *Process) Stop() error {
	// Marking the stop before anything else tells the monitor goroutine not to
	// act on a restart it may already have queued.
	p.mu.Lock()
	p.stoppedExternally = true
	state := p.state
	cmd := p.cmd
	cancel := p.cancel
	monitorDone := p.monitorDone
	pid := p.pid
	p.mu.Unlock()

	// Nothing is running in these states, but the monitor may be sitting in a
	// restart backoff. Canceling the context is what actually stops it.
	if state.IsStopped() || state == StateBackoff {
		p.logger.Info("Process is not running, canceling any pending restart")
		if cancel != nil {
			cancel()
		}
		p.closeLogFiles()
		p.setState(StateStopped)
		return nil
	}

	p.logger.Info("Stopping process", "pid", pid)
	p.setState(StateStopping)

	if cmd != nil && cmd.Process != nil {
		// Check if process is still alive before signaling (it may have exited with parent's SIGTERM)
		if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
			p.logger.Info("Sending SIGINT for graceful shutdown")
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				p.logger.Warn("Failed to send SIGINT", "error", err)
			}
		}

		if monitorDone != nil {
			select {
			case <-monitorDone:
				p.logger.Info("Process exited gracefully")
			case <-time.After(gracefulShutdownTimeout):
				p.logger.Info("Graceful shutdown timeout, sending SIGKILL")
				if err := cmd.Process.Kill(); err != nil {
					p.logger.Warn("Failed to send SIGKILL", "error", err)
				}
				<-monitorDone
				p.logger.Info("Force killed")
			}
		}
	}

	if cancel != nil {
		cancel()
	}

	p.logger.Info("Closing process log files")
	p.closeLogFiles()

	p.logger.Info("Process stopped successfully")
	return nil
}

// Restart restarts the process
func (p *Process) Restart() error {
	p.logger.Info("Restarting process")
	if err := p.Stop(); err != nil {
		p.logger.Error("Error during stop phase of restart", "error", err)
		return err
	}
	p.logger.Debug("Waiting 100ms before restart")
	time.Sleep(restartWaitInterval)
	return p.Start()
}

// monitor waits for the process to exit and applies the restart policy
func (p *Process) monitor(ctx context.Context, cmd *exec.Cmd, done chan struct{}) {
	defer close(done)

	err := cmd.Wait()

	p.mu.Lock()
	p.exitCode = exitCodeOf(cmd, err)
	p.stopTime = time.Now()
	p.lastError = err
	// A run that lasted long enough is treated as successful, so max_restarts
	// bounds consecutive crashes rather than the lifetime restart count.
	if p.stopTime.Sub(p.startTime) >= healthyUptime {
		p.restartCount = 0
	}
	stoppedExternally := p.stoppedExternally
	exitCode := p.exitCode
	p.mu.Unlock()

	switch currentState := p.GetState(); {
	case currentState == StateStopping && stoppedExternally:
		p.logger.Info("Process stopped", "exit_code", exitCode)
		p.setState(StateStopped)
	case currentState == StateStopping:
		p.logger.Info("Process exited during stop", "exit_code", exitCode)
		p.setState(StateExited)
	case stoppedExternally:
		p.logger.Info("Process exited before it was stopped", "exit_code", exitCode)
		p.setState(StateStopped)
	default:
		p.logger.Info("Process exited", "exit_code", exitCode)
		p.setState(StateExited)
		p.maybeRestart(ctx, exitCode)
	}
}

// maybeRestart applies the autorestart policy after an unsupervised exit
func (p *Process) maybeRestart(ctx context.Context, exitCode int) {
	shouldRestart := false
	switch p.config.Autorestart {
	case config.RestartAlways:
		shouldRestart = true
		p.logger.Debug("Autorestart policy is 'always', will restart")
	case config.RestartUnexpected:
		shouldRestart = exitCode != 0
		p.logger.Debug("Autorestart policy is 'unexpected'", "exit_code", exitCode, "will_restart", shouldRestart)
	case config.RestartNever:
		p.logger.Debug("Autorestart policy is 'never', will not restart")
	}

	if !shouldRestart {
		return
	}

	p.mu.Lock()
	if p.restartCount >= p.config.MaxRestarts {
		p.mu.Unlock()
		p.logger.Error("Exceeded maximum restart attempts, setting state to FATAL", "max_restarts", p.config.MaxRestarts)
		p.setState(StateFatal)
		return
	}
	p.restartCount++
	attempt := p.restartCount
	p.mu.Unlock()

	backoff := backoffDuration(attempt)
	p.logger.Info("Restart attempt", "attempt", attempt, "max_restarts", p.config.MaxRestarts, "backoff", backoff)

	select {
	case <-time.After(backoff):
	case <-ctx.Done():
		p.logger.Info("Restart canceled")
		return
	}

	p.mu.RLock()
	canceled := p.stoppedExternally || p.state == StateStopping
	p.mu.RUnlock()
	if canceled {
		p.logger.Info("Restart canceled")
		return
	}

	p.logger.Info("Attempting restart after backoff")
	p.setState(StateBackoff)
	if err := p.Start(); err != nil {
		p.logger.Error("Restart failed", "error", err)
		p.setState(StateFatal)
	}
}

// backoffDuration returns the delay before restart attempt n, counting from 1
func backoffDuration(attempt int) time.Duration {
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	return min(time.Duration(1<<uint(shift))*time.Second, maxBackoffSeconds*time.Second)
}

// exitCodeOf resolves the exit code of a finished command
func exitCodeOf(cmd *exec.Cmd, waitErr error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if waitErr == nil {
		return 0
	}

	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return -1
}

// setupLogFiles sets up log file rotation
func (p *Process) setupLogFiles(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Handles from a previous run would otherwise leak on every restart.
	p.closeLogFilesLocked()

	stdoutPath := p.config.StdoutLogfile
	stderrPath := p.config.StderrLogfile
	p.sharedLogFile = stdoutPath != "" && stderrPath != "" && stdoutPath == stderrPath

	if p.sharedLogFile {
		file, err := openLogFile(stdoutPath)
		if err != nil {
			return fmt.Errorf("failed to open shared log file: %w", err)
		}
		p.stdoutFile = file
		p.stderrFile = file

		maxBytes := max(p.config.StdoutLogfileMaxBytes, p.config.StderrLogfileMaxBytes)
		backups := max(p.config.StdoutLogfileBackups, p.config.StderrLogfileBackups)
		maxAge := max(p.config.StdoutLogfileMaxAge, p.config.StderrLogfileMaxAge)

		p.stdoutRotator = logrotate.NewRotator(stdoutPath, maxBytes, backups, maxAge)
		p.stderrRotator = nil
	} else {
		if stdoutPath != "" {
			file, err := openLogFile(stdoutPath)
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
			file, err := openLogFile(stderrPath)
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

	go p.monitorLogRotation(ctx)

	return nil
}

// openLogFile opens a log file for appending, creating its directory if needed
func openLogFile(path string) (*os.File, error) {
	if dir := getDir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create log directory: %w", err)
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// monitorLogRotation periodically checks and rotates logs
func (p *Process) monitorLogRotation(ctx context.Context) {
	ticker := time.NewTicker(logRotationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.mu.RLock()
			stdoutRotator := p.stdoutRotator
			stderrRotator := p.stderrRotator
			p.mu.RUnlock()

			if stdoutRotator != nil {
				if err := stdoutRotator.CheckAndRotate(); err != nil {
					p.logger.Error("Failed to rotate stdout log", "error", err)
				}
			}
			if stderrRotator != nil {
				if err := stderrRotator.CheckAndRotate(); err != nil {
					p.logger.Error("Failed to rotate stderr log", "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// closeLogFiles closes log file handles
func (p *Process) closeLogFiles() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLogFilesLocked()
}

// closeLogFilesLocked closes log file handles. Must be called with mu held.
func (p *Process) closeLogFilesLocked() {
	if p.stdoutFile != nil {
		if err := p.stdoutFile.Close(); err != nil {
			p.logger.Warn("failed to close stdout log file", "error", err)
		}
		if p.sharedLogFile {
			// Both streams share one handle, so it must not be closed twice
			p.stderrFile = nil
		}
		p.stdoutFile = nil
	}
	if p.stderrFile != nil {
		if err := p.stderrFile.Close(); err != nil {
			p.logger.Warn("failed to close stderr log file", "error", err)
		}
		p.stderrFile = nil
	}

	p.stdoutRotator = nil
	p.stderrRotator = nil
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
	p.mu.RLock()
	cmd := p.cmd
	p.mu.RUnlock()

	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("process is not running")
	}
	return cmd.Process.Signal(sig)
}
