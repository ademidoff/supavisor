package server

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/dependency"
	"github.com/ademidoff/supavisor/internal/process"
)

const (
	// actionTimeout bounds how long a start or stop request waits for the
	// reconciler to produce a settled outcome before reporting back.
	actionTimeout = 30 * time.Second
	pollInterval  = 100 * time.Millisecond
)

// ProcessStatusInfo contains status information about a process
type ProcessStatusInfo struct {
	Name         string
	State        process.State
	Uptime       string
	PID          int
	ExitCode     int
	RestartCount int
}

// Server manages all processes
type Server struct {
	config          *config.Config
	logger          *slog.Logger
	processLogger   *slog.Logger
	processes       map[string]*process.Process
	desired         map[string]DesiredState
	inflight        map[string]bool
	dependencyGraph *dependency.Graph
	ipcServer       *IPCServer
	pidLock         *pidLock
	stateDirty      chan struct{}
	reconcileNow    chan struct{}
	reconcileDone   chan struct{}
	stopChan        chan struct{}
	stateFile       string
	actions         sync.WaitGroup
	processMutex    sync.RWMutex
	running         bool
}

// New creates a new server instance
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if err := cfg.EnsureLogDirectories(); err != nil {
		return nil, fmt.Errorf("failed to create log directories: %w", err)
	}

	// Build dependency graph
	graph := dependency.NewGraph()
	for name, progConfig := range cfg.Programs {
		graph.AddNode(name, progConfig.DependsOn)
	}

	// Verify no circular dependencies
	if _, err := graph.TopologicalSort(); err != nil {
		return nil, fmt.Errorf("dependency graph validation failed: %w", err)
	}

	s := &Server{
		config:          cfg,
		logger:          logger.With("component", "main"),
		processLogger:   logger,
		processes:       make(map[string]*process.Process, len(cfg.Programs)),
		desired:         make(map[string]DesiredState, len(cfg.Programs)),
		inflight:        make(map[string]bool),
		dependencyGraph: graph,
		stopChan:        make(chan struct{}),
		stateDirty:      make(chan struct{}, 1),
		reconcileNow:    make(chan struct{}, 1),
		reconcileDone:   make(chan struct{}),
		stateFile:       stateFilePath(cfg.Supavisor.PidFile),
	}

	// Every configured program gets a process up front, running or not, so that
	// the reconciler and the status command can see the whole set rather than
	// only what has been started so far.
	for name, progConfig := range cfg.Programs {
		proc := process.NewProcess(progConfig, s.processLogger)
		proc.SetStateChangeCallback(s.onProcessStateChange)
		s.processes[name] = proc

		s.desired[name] = DesiredStopped
		if progConfig.Autostart {
			s.desired[name] = DesiredRunning
		}
	}

	return s, nil
}

// Start starts the supavisor
func (s *Server) Start() error {
	if s.running {
		return fmt.Errorf("supavisor is already running")
	}

	// Holding this for the lifetime of the daemon is what keeps a second
	// instance from starting, and it is dropped by the kernel on a crash.
	if err := s.lockPIDFile(); err != nil {
		return err
	}

	// Holding the lock means no other daemon is running, so anything still
	// alive from a previous one is an orphan of a crash.
	s.reapOrphans()

	s.running = true

	// Start IPC server
	s.ipcServer = NewIPCServer(s.config.Supavisor.Socket, s.config.Supavisor.SocketGroup, s)
	if err := s.ipcServer.Start(); err != nil {
		s.releasePIDFile()
		s.running = false
		return fmt.Errorf("failed to start IPC server: %w", err)
	}

	// Setup signal handling
	s.setupSignalHandling()

	go s.recordStateChanges()

	s.logger.Info("IPC server started", "socket", s.config.Supavisor.Socket)
	s.logger.Info("Starting processes...")

	go s.reconcileLoop()

	s.logger.Info("Supavisor started successfully")
	return nil
}

// Stop stops the supavisor and all processes
func (s *Server) Stop() error {
	if !s.running {
		return nil
	}

	s.logger.Info("Stopping supavisor...")
	s.running = false
	close(s.stopChan)

	// Let the reconciler and anything it started settle first, or it would
	// bring processes straight back up as they are stopped.
	<-s.reconcileDone
	s.actions.Wait()

	s.stopAllProcesses()

	// Stop IPC server
	if s.ipcServer != nil {
		s.logger.Info("Stopping IPC server")
		if err := s.ipcServer.Stop(); err != nil {
			s.logger.Error("failed to stop IPC server", "error", err)
		}
	}

	// Everything was stopped deliberately, so there is nothing for the next
	// daemon to reap.
	if s.stateFile != "" {
		s.clearStateFile()
	}

	s.releasePIDFile()

	s.logger.Info("Supavisor daemon stopped")
	return nil
}

// stopAllProcesses stops every process, working from the outermost dependents
// inwards and stopping each tier in parallel.
//
// Stopping serially cost stopwaitsecs for every process that does not exit on
// its stop signal, so a handful of them was enough to exceed systemd's
// TimeoutStopSec and have the daemon killed with its processes still running.
// Order matters as well: stopping a database before the programs using it is
// the wrong way round.
func (s *Server) stopAllProcesses() {
	tiers, err := s.dependencyGraph.Tiers()
	if err != nil {
		s.logger.Warn("Failed to order shutdown, stopping everything at once", "error", err)
		tiers = [][]string{s.programNames()}
	}

	for tier := len(tiers) - 1; tier >= 0; tier-- {
		names := tiers[tier]
		s.logger.Info("Stopping processes", "tier", tier, "count", len(names), "processes", names)

		var wg sync.WaitGroup
		for _, name := range names {
			proc := s.process(name)
			if proc == nil {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				if stopErr := proc.Stop(); stopErr != nil {
					s.logger.Warn("failed to stop process", "process", name, "error", stopErr)
				}
			}()
		}
		wg.Wait()
	}
}

// StartProcess marks a process as wanted and reports whether it came up
func (s *Server) StartProcess(name string) error {
	s.logger.Info("Start requested", "process", name)

	proc := s.process(name)
	if proc == nil {
		return fmt.Errorf("process %s not found", name)
	}
	if proc.GetState() == process.StateRunning {
		return fmt.Errorf("process %s is already running", name)
	}

	// A process that gave up, or exited on its own, has to be released back to
	// STOPPED before the reconciler will consider starting it again.
	if proc.GetState().IsStopped() {
		if err := proc.Stop(); err != nil {
			s.logger.Warn("failed to reset process before start", "process", name, "error", err)
		}
	}

	if err := s.setDesired(name, DesiredRunning); err != nil {
		return err
	}
	s.requestReconcile()

	return s.awaitState(name, func(state process.State) bool {
		return state == process.StateRunning
	})
}

// StopProcess marks a process as unwanted and waits for it to stop
func (s *Server) StopProcess(name string) error {
	s.logger.Info("Stop requested", "process", name)

	if err := s.setDesired(name, DesiredStopped); err != nil {
		return err
	}
	s.requestReconcile()

	return s.awaitState(name, func(state process.State) bool {
		return state == process.StateStopped
	})
}

// RestartProcess stops a process and starts it again
func (s *Server) RestartProcess(name string) error {
	s.logger.Info("Restart requested", "process", name)

	if err := s.StopProcess(name); err != nil {
		return err
	}
	return s.StartProcess(name)
}

// Reload reloads the configuration
func (s *Server) Reload() error {
	// For now, just validate the current config
	// Full reload would require stopping and restarting processes
	return s.config.Validate()
}

// GetStatus returns the status of all processes
func (s *Server) GetStatus() []ProcessStatusInfo {
	s.processMutex.RLock()
	defer s.processMutex.RUnlock()

	statuses := make([]ProcessStatusInfo, 0, len(s.processes))
	for name, proc := range s.processes {
		state := proc.GetState()
		pid := proc.GetPID()
		exitCode := proc.GetExitCode()
		restartCount := proc.GetRestartCount()

		var uptime string
		if state == process.StateRunning {
			startTime := proc.GetStartTime()
			duration := time.Since(startTime)
			uptime = formatDuration(duration)
		} else {
			uptime = "N/A"
		}

		statuses = append(statuses, ProcessStatusInfo{
			Name:         name,
			State:        state,
			PID:          pid,
			ExitCode:     exitCode,
			RestartCount: restartCount,
			Uptime:       uptime,
		})
	}

	// Sort by process name alphabetically
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	return statuses
}

// onProcessStateChange is called when a process state changes
func (s *Server) onProcessStateChange(name string, prevState, newState process.State) {
	if prevState != newState {
		s.logger.Info("Process state changed", "process", name, "prev_state", prevState, "new_state", newState)
	}
	s.markStateDirty()

	// A dependency reaching RUNNING is what unblocks everything behind it, so
	// reconcile now rather than waiting up to a tick for it.
	s.requestReconcile()
}

// setupSignalHandling sets up signal handling for graceful shutdown
func (s *Server) setupSignalHandling() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		s.logger.Info("Received signal to stop supavisor", "signal", sig.String())
		if err := s.Stop(); err != nil {
			s.logger.Error("failed to stop supavisor", "error", err)
		}
		os.Exit(0)
	}()
}

// lockPIDFile takes the exclusive PID file lock for this daemon
func (s *Server) lockPIDFile() error {
	if s.config.Supavisor.PidFile == "" {
		s.logger.Warn("No pidfile configured: nothing prevents a second instance from starting")
		return nil
	}

	lock, err := acquirePIDLock(s.config.Supavisor.PidFile)
	if err != nil {
		return err
	}
	s.pidLock = lock
	return nil
}

// releasePIDFile drops the PID file lock and removes the file
func (s *Server) releasePIDFile() {
	if s.pidLock == nil {
		return
	}
	if err := s.pidLock.Release(); err != nil {
		s.logger.Warn("failed to release PID file", "error", err)
	}
	s.pidLock = nil
}

// formatDuration formats a duration as a human-readable string
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	} else {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		seconds := int(d.Seconds()) % 60
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
}
