package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
	maxBackoffSeconds   = 30
	restartWaitInterval = 100 * time.Millisecond

	// logDrainTimeout bounds how long we wait for a pipe to reach EOF after the
	// process exits. A grandchild that inherited the descriptor keeps it open,
	// so the wait cannot be unbounded.
	logDrainTimeout = 2 * time.Second

	// maxLogLineBytes bounds how much of a newline-free run of output is held in
	// memory before it is written out in pieces.
	maxLogLineBytes = 64 * 1024

	// maxBackoffShift caps the exponent used for restart backoff. Without it
	// 1<<(restartCount-1) overflows int once max_restarts grows past ~63 and
	// yields a zero backoff, turning the restart policy into a tight loop.
	maxBackoffShift = 5

	// reapGrace bounds the wait for a monitor to report an exit after SIGKILL.
	// A process in an uninterruptible wait does not die on SIGKILL, and one
	// program in that state must not be able to hold up the daemon's shutdown.
	reapGrace = 5 * time.Second

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

	// onStateChange is set once before the process is started
	onStateChange func(name string, prevState, newState State)

	// onHealthChange is set once before the process is started
	onHealthChange func(name string, prevHealth, health Health)

	cmd           *exec.Cmd
	cancel        context.CancelFunc
	healthCancel  context.CancelFunc
	monitorDone   chan struct{}
	stopRequested chan struct{}
	healthDone    chan struct{}
	lastError     error
	startTime     time.Time
	stopTime      time.Time
	state         State
	health        Health
	streams       []*logStream

	// mu guards all mutable run state, from cmd down to stoppedExternally.
	// The monitor goroutine writes it while the IPC path reads it.
	mu                sync.RWMutex
	pid               int
	exitCode          int
	restartCount      int
	completed         bool
	stoppedExternally bool
}

// logStream captures one child output stream. The child writes into a pipe and
// a drain goroutine copies it into the rotating log file, so that supavisor
// rather than the child owns the log descriptor.
type logStream struct {
	sink     *logrotate.Writer
	readEnd  *os.File
	childEnd *os.File
	done     chan struct{}
}

// NewProcess creates a new process instance
func NewProcess(cfg *config.ProgramConfig, logger *slog.Logger) *Process {
	return &Process{
		config: cfg,
		logger: logger.With("component", "process", "process", cfg.Name),
		state:  StateStopped,
		health: HealthNone,
	}
}

// SetStateChangeCallback sets a callback for state changes
func (p *Process) SetStateChangeCallback(fn func(name string, prevState, newState State)) {
	p.onStateChange = fn
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

// HasCompleted reports whether the program has finished its work successfully:
// a run that ended on its own with status 0.
//
// It stays true while the program sits in EXITED, which is what lets a one-off
// be depended on. Starting the program again clears it, so the answer is always
// about the most recent run rather than about any run there has ever been.
func (p *Process) HasCompleted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.completed
}

// Start starts the process
func (p *Process) Start() error {
	p.mu.Lock()
	if p.state == StateRunning || p.state == StateStarting {
		p.mu.Unlock()
		return fmt.Errorf("process %s is already running or starting", p.config.Name)
	}
	// Running again puts whatever the last run achieved back in question, so
	// anything waiting for this program to complete waits for this run instead.
	p.completed = false
	// Every run gets a fresh context, canceled by Stop(). It drives this run's
	// health checks and restart backoff, so reusing a canceled one would leave
	// a restarted process with neither.
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	p.logger.Info("Setting state to STARTING")
	p.setState(StateStarting)

	p.logger.Info("Setting up log capture")
	stdout, stderr, err := p.startLogging()
	if err != nil {
		p.stopLogging()
		cancel()
		p.setState(StateFatal)
		return fmt.Errorf("failed to set up log capture: %w", err)
	}

	cmd, err := p.buildCommand(stdout, stderr)
	if err != nil {
		p.stopLogging()
		cancel()
		p.setState(StateFatal)
		return err
	}

	p.logger.Info("Executing command", "command", cmd.String())
	exited, err := p.spawn(cmd)
	if err != nil {
		p.logger.Error("Failed to start", "error", err)
		p.stopLogging()
		cancel()
		p.setState(StateFatal)
		return fmt.Errorf("failed to start process: %w", err)
	}

	// The child has its own copies now. Until ours are closed the pipes never
	// reach EOF and the drain goroutines would never finish.
	p.closeChildEnds()

	monitorDone := make(chan struct{})
	stopRequested := make(chan struct{})
	// Closed as soon as the monitor sees the exit, so the start check can tell
	// a process that fell over from one that is still up without probing a PID
	// that may already have been reused.
	runExited := make(chan struct{})

	pid := p.recordRun(cmd, monitorDone, stopRequested)
	p.logger.Info("Started process", "pid", pid)

	// Before the monitor: it tears the checker down when the process exits, and
	// a process that exits straight away would otherwise leave one running.
	p.startHealthChecks(ctx)

	go p.monitor(ctx, cmd, exited, runExited, monitorDone, stopRequested)
	go p.waitForStartSuccess(ctx, runExited)

	return nil
}

// recordRun publishes the state of a run that has just started, and returns its
// PID. Everything here is read by the IPC path while the monitor writes it.
func (p *Process) recordRun(cmd *exec.Cmd, monitorDone, stopRequested chan struct{}) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.startTime = time.Now()
	p.lastError = nil
	p.monitorDone = monitorDone
	p.stopRequested = stopRequested
	p.stoppedExternally = false
	return p.pid
}

// spawn starts the command through the reaper, which registers the PID before
// it can exit. Starting it any other way would let a program that fails
// immediately have its status collected with nobody yet listening for it.
func (p *Process) spawn(cmd *exec.Cmd) (<-chan syscall.WaitStatus, error) {
	return reaperFor(p.logger).spawn(func() (int, error) {
		err := cmd.Start()
		if err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
}

// buildCommand assembles the exec.Cmd for a single run. A nil stdout or stderr
// descriptor leaves the stream connected to /dev/null.
func (p *Process) buildCommand(stdout, stderr *os.File) (*exec.Cmd, error) {
	p.logger.Debug("Parsing command", "command", p.config.Command)
	parts := parseCommand(p.config.Command)
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid command: %s", p.config.Command)
	}

	p.logger.Debug("Creating command", "command_parts", parts)
	// Deliberately not CommandContext: its context watcher is wired to Wait,
	// which the reaper now owns, so it would never be released. Its Cancel hook
	// would also fire against a PID that has already been reaped and may have
	// been recycled by then, killing an unrelated process group. Stop signals
	// the group explicitly, so nothing is lost.
	//nolint:gosec,noctx // noctx wants CommandContext; see the comment above for why it cannot be used here
	cmd := exec.Command(parts[0], parts[1:]...)

	// Give the child its own process group, which everything it spawns
	// inherits. Signaling the group is the only way to reach the workload
	// behind a wrapper script: killing just the direct child leaves its
	// children running and reparented to init.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if p.config.Directory != "" {
		p.logger.Info("Setting working directory", "directory", p.config.Directory)
		cmd.Dir = p.config.Directory
	}

	if len(p.config.Environment) > 0 {
		p.logger.Info("Setting environment variables", "count", len(p.config.Environment))
	}
	cmd.Env = processEnv(p.config)

	// Leave Stdout/Stderr nil when there is no capture pipe: assigning a nil
	// *os.File would hand the child a closed descriptor instead of /dev/null.
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}

	return cmd, nil
}

// processEnv returns the environment a program runs with, which its health
// check probe also inherits
func processEnv(cfg *config.ProgramConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

// waitForStartSuccess promotes the process to RUNNING once it has stayed alive
// for startsecs.
func (p *Process) waitForStartSuccess(ctx context.Context, runExited <-chan struct{}) {
	p.logger.Info("Waiting before checking start success", "seconds", p.config.StartSecs)

	select {
	case <-time.After(time.Duration(p.config.StartSecs) * time.Second):
	case <-ctx.Done():
		return
	case <-runExited:
		// The monitor owns the state from here: it has the exit code and the
		// restart policy, and will settle on BACKOFF, EXITED or FATAL
		// accordingly. Setting a state here as well would publish a start
		// failure for a program that exited cleanly, and would race the
		// monitor for a transition it is about to make correctly.
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
	// Taking the channel makes sure it is closed exactly once, however many
	// times Stop is called.
	stopRequested := p.stopRequested
	p.stopRequested = nil
	p.mu.Unlock()

	// Wake anything waiting out a restart backoff before doing anything else,
	// so a stop is not held up by a delay that is about to be abandoned.
	if stopRequested != nil {
		close(stopRequested)
	}

	// A probe must not outlive the process it is checking, and readiness means
	// nothing for a program that is going away.
	p.stopHealthChecks()

	// Nothing is running in these states, but the monitor may be sitting in a
	// restart backoff. Canceling the context is what actually stops it.
	if state.IsStopped() || state == StateBackoff {
		p.logger.Info("Process is not running, canceling any pending restart")
		if cancel != nil {
			cancel()
		}
		p.stopLogging()
		p.setState(StateStopped)
		return nil
	}

	p.logger.Info("Stopping process", "pid", pid)
	p.setState(StateStopping)

	if cmd != nil && cmd.Process != nil {
		// Check the group is still alive before signaling: the process may have
		// exited already, for instance on the parent's own SIGTERM.
		if err := SignalGroup(pid, syscall.Signal(0)); err == nil {
			p.logger.Info("Signaling the process group for graceful shutdown", "signal", p.config.StopSignal)
			if err := SignalGroup(pid, p.config.StopSignal); err != nil {
				p.logger.Warn("Failed to send stop signal", "signal", p.config.StopSignal, "error", err)
			}
		}

		if monitorDone != nil {
			select {
			case <-monitorDone:
				p.logger.Info("Process exited gracefully")
			case <-time.After(time.Duration(p.config.StopWaitSecs) * time.Second):
				p.logger.Info("Graceful shutdown timeout, sending SIGKILL to the process group")
				if err := SignalGroup(pid, syscall.SIGKILL); err != nil {
					p.logger.Warn("Failed to send SIGKILL", "error", err)
				}

				// Bounded, because SIGKILL is not always the end of it: a
				// process wedged in an uninterruptible wait does not die on it,
				// and waiting forever would take the whole shutdown down with
				// this one program. Giving up here leaves the run unfinished,
				// which the reconciler will see, rather than hanging the daemon.
				if awaitClose(monitorDone, reapGrace) {
					p.logger.Info("Force killed")
				} else {
					p.logger.Error("Process did not report its exit after SIGKILL, giving up on it",
						"pid", pid, "waited", reapGrace)
				}
			}
		}
	}

	if cancel != nil {
		cancel()
	}

	p.logger.Info("Closing process log files")
	p.stopLogging()

	p.logger.Info("Process stopped successfully")
	return nil
}

// SignalGroup sends sig to the process group led by pid. Children inherit their
// parent's group, so this reaches the whole tree rather than just the process
// supavisor started.
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return syscall.Kill(-pid, sig)
}

// killLingeringGroup kills anything left in the process group after the process
// itself has exited
func (p *Process) killLingeringGroup(pid int) {
	// The leader has been reaped by the time this runs, so its PID is free and
	// the group id is only ours for as long as nothing else claims that number.
	// A live process holding it means it has been handed out again, and
	// signaling the group could then reach a stranger's tree rather than our
	// leftovers. Refusing to clean up in that case leaks whatever is left in
	// the old group, which is the lesser of the two: it is bounded by the
	// program's own children, where the alternative is unbounded.
	if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
		p.logger.Warn("Not clearing the process group: its id belongs to a live process now", "pgid", pid)
		return
	}

	if err := SignalGroup(pid, syscall.Signal(0)); err != nil {
		return
	}

	p.logger.Info("Process group still has members after exit, killing them", "pgid", pid)
	if err := SignalGroup(pid, syscall.SIGKILL); err != nil {
		p.logger.Warn("Failed to kill lingering process group", "pgid", pid, "error", err)
	}
}

// awaitClose waits for done to be closed, and reports whether it was before the
// timeout elapsed.
func awaitClose(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
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
func (p *Process) monitor(
	ctx context.Context,
	cmd *exec.Cmd,
	exited <-chan syscall.WaitStatus,
	runExited, done, stopRequested chan struct{},
) {
	defer close(done)

	// The status comes from the reaper rather than cmd.Wait(): one waiter for
	// the whole daemon is what keeps orphan reaping from stealing it.
	status := <-exited
	close(runExited)

	// Read before releasing the handle below, which sets Process.Pid to -1.
	pid := cmd.Process.Pid

	// The process is reaped, but anything it spawned is still in its group and
	// would survive as an orphan. Clear the group first: those grandchildren
	// also hold the log pipe open, so removing them lets the drain finish
	// instead of timing out.
	p.killLingeringGroup(pid)

	// Nothing waited on the os.Process, so the handle Start allocated is still
	// open. Releasing it closes the pidfd, which would otherwise leak one
	// descriptor per run.
	releaseErr := cmd.Process.Release()
	if releaseErr != nil {
		p.logger.Debug("Failed to release the process handle", "error", releaseErr)
	}

	// Probing a process that has exited would keep reporting on whatever else
	// answers at that address until something stops the checker.
	p.stopHealthChecks()

	// Flush whatever the process left in the pipes before recording the exit.
	p.stopLogging()

	p.mu.Lock()
	p.exitCode = exitCodeOfStatus(status)
	p.stopTime = time.Now()
	p.lastError = exitErrorOf(status)
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
		// A clean exit nobody asked for is the program finishing its work.
		// Latched before the state change, because that change is what wakes
		// anything waiting for this program to complete.
		if exitCode == 0 {
			p.mu.Lock()
			p.completed = true
			p.mu.Unlock()
		}
		p.setState(StateExited)
		p.maybeRestart(ctx, stopRequested, exitCode)
	}
}

// maybeRestart applies the autorestart policy after an unsupervised exit
func (p *Process) maybeRestart(ctx context.Context, stopRequested chan struct{}, exitCode int) {
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
	case <-stopRequested:
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

// exitErrorOf describes a non-successful exit, and is nil for a clean one. It
// stands in for the error cmd.Wait() used to return, which callers surface as
// the program's last error.
func exitErrorOf(status syscall.WaitStatus) error {
	switch {
	case status.Signaled():
		return fmt.Errorf("signal: %s", status.Signal())
	case status.ExitStatus() != 0:
		return fmt.Errorf("exit status %d", status.ExitStatus())
	default:
		return nil
	}
}

// startLogging creates this run's capture pipes and returns the descriptors to
// hand to the child. A nil descriptor means the stream is discarded.
func (p *Process) startLogging() (stdout, stderr *os.File, err error) {
	stdoutPath := p.config.StdoutLogfile
	stderrPath := p.config.StderrLogfile

	var stream *logStream

	// One file means one pipe, so the two streams interleave in write order
	// instead of racing two writers against the same path.
	if stdoutPath != "" && stdoutPath == stderrPath {
		stream, err = p.addLogStream(
			stdoutPath,
			max(p.config.StdoutLogfileMaxBytes, p.config.StderrLogfileMaxBytes),
			max(p.config.StdoutLogfileBackups, p.config.StderrLogfileBackups),
			max(p.config.StdoutLogfileMaxAge, p.config.StderrLogfileMaxAge),
		)
		if err != nil {
			return nil, nil, err
		}
		return stream.childEnd, stream.childEnd, nil
	}

	if stdoutPath != "" {
		stream, err = p.addLogStream(
			stdoutPath,
			p.config.StdoutLogfileMaxBytes,
			p.config.StdoutLogfileBackups,
			p.config.StdoutLogfileMaxAge,
		)
		if err != nil {
			return nil, nil, err
		}
		stdout = stream.childEnd
	}
	if stderrPath != "" {
		stream, err = p.addLogStream(
			stderrPath,
			p.config.StderrLogfileMaxBytes,
			p.config.StderrLogfileBackups,
			p.config.StderrLogfileMaxAge,
		)
		if err != nil {
			return nil, nil, err
		}
		stderr = stream.childEnd
	}

	return stdout, stderr, nil
}

// addLogStream opens a rotating log file and starts draining a pipe into it
func (p *Process) addLogStream(path string, maxBytes int64, backups, maxAge int) (*logStream, error) {
	sink, err := logrotate.NewWriter(path, maxBytes, backups, maxAge)
	if err != nil {
		return nil, err
	}

	readEnd, childEnd, err := os.Pipe()
	if err != nil {
		_ = sink.Close()
		return nil, fmt.Errorf("failed to create log pipe for %s: %w", path, err)
	}

	stream := &logStream{
		sink:     sink,
		readEnd:  readEnd,
		childEnd: childEnd,
		done:     make(chan struct{}),
	}

	p.mu.Lock()
	p.streams = append(p.streams, stream)
	p.mu.Unlock()

	go func() {
		defer close(stream.done)
		if err := drainStream(stream.sink, stream.readEnd); err != nil {
			p.logger.Warn("Log capture ended with an error", "path", path, "error", err)
		}
	}()

	return stream, nil
}

// drainStream copies the pipe into the log one line at a time. Copying in bulk
// would hand the writer chunks far larger than the rotation threshold, so the
// log would overshoot maxbytes by a whole read buffer on every rotation.
func drainStream(sink io.Writer, pipe io.Reader) error {
	reader := bufio.NewReaderSize(pipe, maxLogLineBytes)

	for {
		line, err := reader.ReadSlice('\n')
		if len(line) > 0 {
			if _, writeErr := sink.Write(line); writeErr != nil {
				return writeErr
			}
		}

		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			// A line longer than the buffer is written in pieces
			continue
		case errors.Is(err, io.EOF), errors.Is(err, os.ErrClosed):
			return nil
		default:
			return err
		}
	}
}

// closeChildEnds releases the parent's copy of each pipe write end, which the
// child inherited during Start
func (p *Process) closeChildEnds() {
	p.mu.RLock()
	streams := p.streams
	p.mu.RUnlock()

	for _, stream := range streams {
		if stream.childEnd != nil {
			_ = stream.childEnd.Close()
			stream.childEnd = nil
		}
	}
}

// stopLogging drains what the process left in the pipes and closes the log
// files. It is safe to call more than once.
func (p *Process) stopLogging() {
	p.mu.Lock()
	streams := p.streams
	p.streams = nil
	p.mu.Unlock()

	for _, stream := range streams {
		if stream.childEnd != nil {
			_ = stream.childEnd.Close()
			stream.childEnd = nil
		}

		// A pipe only reaches EOF once every writer has closed it, and a
		// grandchild that outlived the process still holds one, so the drain
		// gets a bounded window before we take the read end away from it.
		select {
		case <-stream.done:
		case <-time.After(logDrainTimeout):
			p.logger.Warn("Log capture still open after exit, closing it", "path", stream.sink.Path())
			_ = stream.readEnd.Close()
			<-stream.done
		}

		_ = stream.readEnd.Close()
		if err := stream.sink.Close(); err != nil {
			p.logger.Warn("Failed to close log file", "path", stream.sink.Path(), "error", err)
		}
	}
}

// parseCommand splits a command string into its arguments.
//
// This is not a shell: there is no expansion, globbing or substitution. It
// handles quoting and backslash escapes so that arguments containing spaces,
// quotes or an empty string can be expressed.
func parseCommand(cmd string) []string {
	parts := []string{}
	scan := commandScanner{}

	for i := 0; i < len(cmd); i++ {
		// A backslash escapes the next character, except inside single quotes
		// where a shell treats it literally too.
		if cmd[i] == '\\' && scan.quoteChar != '\'' && i+1 < len(cmd) {
			i++
			scan.current = append(scan.current, cmd[i])
			continue
		}
		if arg, complete := scan.step(cmd[i]); complete {
			parts = append(parts, arg)
		}
	}

	if arg, complete := scan.finish(); complete {
		parts = append(parts, arg)
	}

	return parts
}

// commandScanner tracks quoting while splitting a command string
type commandScanner struct {
	current   []byte
	quoteChar byte
	inQuotes  bool
	// quoted records that this argument was written with quotes, so that ""
	// produces an empty argument rather than nothing at all.
	quoted bool
}

// step consumes one character, returning an argument when one is complete
func (s *commandScanner) step(char byte) (arg string, complete bool) {
	switch {
	case (char == '"' || char == '\'') && !s.inQuotes:
		s.inQuotes = true
		s.quoteChar = char
		s.quoted = true

	case s.inQuotes && char == s.quoteChar:
		s.inQuotes = false
		s.quoteChar = 0

	case (char == ' ' || char == '\t') && !s.inQuotes:
		return s.finish()

	default:
		s.current = append(s.current, char)
	}

	return "", false
}

// finish closes off the argument being scanned, if there is one
func (s *commandScanner) finish() (arg string, complete bool) {
	if len(s.current) == 0 && !s.quoted {
		return "", false
	}

	arg = string(s.current)
	s.current = s.current[:0]
	s.quoted = false
	return arg, true
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
