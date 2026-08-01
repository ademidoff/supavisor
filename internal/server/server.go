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
	dependencyTimeoutSeconds = 30
	monitorInterval          = 5 * time.Second
	pollInterval             = 100 * time.Millisecond
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
	dependencyGraph *dependency.Graph
	ipcServer       *IPCServer
	pidLock         *pidLock
	stateDirty      chan struct{}
	stopChan        chan struct{}
	stateFile       string
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

	return &Server{
		config:          cfg,
		logger:          logger.With("component", "main"),
		processLogger:   logger,
		processes:       make(map[string]*process.Process),
		dependencyGraph: graph,
		stopChan:        make(chan struct{}),
		stateDirty:      make(chan struct{}, 1),
		stateFile:       stateFilePath(cfg.Supavisor.PidFile),
	}, nil
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
	s.ipcServer = NewIPCServer(s.config.Supavisor.Socket, s)
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

	// Start processes that should autostart
	s.startAutostartProcesses()

	// Monitor processes
	go s.monitorProcesses()

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

	// Stop all processes
	s.processMutex.Lock()
	processCount := len(s.processes)
	s.logger.Info("Stopping processes...", "count", processCount)
	for _, proc := range s.processes {
		if err := proc.Stop(); err != nil {
			s.logger.Warn("failed to stop process", "error", err)
		}
	}
	s.processMutex.Unlock()

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

// startAutostartProcesses starts all processes configured to autostart
func (s *Server) startAutostartProcesses() {
	// Get topological sort order
	order, err := s.dependencyGraph.TopologicalSort()
	if err != nil {
		s.logger.Warn("Failed to get startup order", "error", err)
		// Start processes in config order
		for name, progConfig := range s.config.Programs {
			if progConfig.Autostart {
				if err := s.StartProcess(name); err != nil {
					s.logger.Error("failed to start process", "process", name, "error", err)
				}
			}
		}
		return
	}

	// Start processes in dependency order
	for _, name := range order {
		progConfig, exists := s.config.Programs[name]
		if !exists {
			continue
		}

		if progConfig.Autostart {
			s.logger.Info("Starting process (autostart enabled)", "process", name)
			if err := s.StartProcess(name); err != nil {
				s.logger.Error("Failed to start process", "process", name, "error", err)
			} else {
				s.logger.Info("Process started successfully", "process", name)
				// Give the process a moment to transition from STARTING to RUNNING
				// This helps dependent processes that check immediately
				time.Sleep(pollInterval)
			}
		}
	}
}

// waitForSingleDependency waits for a single dependency to be running
func (s *Server) waitForSingleDependency(dep string) error {
	// Wait up to dependencyTimeoutSeconds for the dependency to be running
	timeout := time.After(dependencyTimeoutSeconds * time.Second)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for dependency %s", dep)
		case <-ticker.C:
			s.processMutex.RLock()
			depProc, exists := s.processes[dep]
			if !exists {
				s.processMutex.RUnlock()
				// Process doesn't exist yet, wait for it to be created
				continue
			}
			state := depProc.GetState()
			s.processMutex.RUnlock()

			if state == process.StateRunning {
				return nil
			}
			// If it's not Starting or Running, it failed
			if state != process.StateStarting {
				return fmt.Errorf("dependency %s is in state %s", dep, state)
			}
			// Still starting, wait more
		}
	}
}

// StartProcess starts a specific process
func (s *Server) StartProcess(name string) error { //nolint:gocyclo
	progConfig, exists := s.config.Programs[name]
	if !exists {
		return fmt.Errorf("process %s not found", name)
	}

	s.logger.Info("Starting process", "process", name)

	s.processMutex.Lock()
	defer s.processMutex.Unlock()

	// Check if already exists
	if _, exists := s.processes[name]; exists {
		// Process already exists, check if it's running
		proc := s.processes[name]
		if proc.GetState() == process.StateRunning {
			s.logger.Info("Process is already running", "process", name)
			return fmt.Errorf("process %s is already running", name)
		}
		// Stop existing process if needed
		s.logger.Info("Stopping existing process before restart", "process", name)
		if err := proc.Stop(); err != nil {
			s.logger.Warn("failed to stop existing process", "process", name, "error", err)
		}
	}

	// Check dependencies and wait for them to be running if they're starting
	deps := s.dependencyGraph.GetDependencies(name)
	if len(deps) > 0 {
		s.logger.Info("Process has dependencies", "process", name, "count", len(deps), "dependencies", deps)
	}
	for _, dep := range deps {
		depProc, exists := s.processes[dep]
		if !exists {
			// Dependency doesn't exist yet, wait for it to be created and running
			s.logger.Info("Waiting for dependency to start", "process", name, "dependency", dep)
			// Release the lock while waiting to avoid deadlock
			s.processMutex.Unlock()
			if err := s.waitForSingleDependency(dep); err != nil {
				s.processMutex.Lock()
				return fmt.Errorf("dependency %s failed to start: %w", dep, err)
			}
			s.processMutex.Lock()
			// Re-check after waiting
			depProc, exists = s.processes[dep]
			if !exists || depProc.GetState() != process.StateRunning {
				return fmt.Errorf("dependency %s is not running", dep)
			}
			s.logger.Info("Dependency is now running", "dependency", dep)
			continue
		}
		state := depProc.GetState()
		if state == process.StateStarting {
			s.logger.Info("Waiting for dependency to finish starting", "process", name, "dependency", dep)
			// Dependency is starting, wait for it to become running
			// Release the lock while waiting to avoid deadlock
			s.processMutex.Unlock()
			if err := s.waitForSingleDependency(dep); err != nil {
				s.processMutex.Lock()
				return fmt.Errorf("dependency %s failed to start: %w", dep, err)
			}
			s.processMutex.Lock()
			// Re-check the state after waiting
			depProc, exists = s.processes[dep]
			if !exists || depProc.GetState() != process.StateRunning {
				return fmt.Errorf("dependency %s is not running", dep)
			}
			s.logger.Info("Dependency is now running", "process", name, "dependency", dep)
		} else if state != process.StateRunning {
			return fmt.Errorf("dependency %s is not running (state: %s)", dep, state)
		}
	}

	return s.createAndStartProcess(name, progConfig)
}

// createAndStartProcess creates, starts, and registers a new process instance.
// Must be called with processMutex held.
func (s *Server) createAndStartProcess(name string, progConfig *config.ProgramConfig) error {
	s.logger.Info("Creating process instance", "process", name)
	proc := process.NewProcess(progConfig, s.processLogger)
	proc.SetStateChangeCallback(s.onProcessStateChange)
	proc.SetDependencyStopCallback(s.onDependencyStop)

	s.logger.Info("Calling Start()", "process", name)
	if err := proc.Start(); err != nil {
		s.logger.Error("Failed to start process", "process", name, "error", err)
		return fmt.Errorf("failed to start process: %w", err)
	}

	s.processes[name] = proc
	s.logger.Info("Process started", "process", name, "pid", proc.GetPID())
	return nil
}

// StopProcess stops a specific process
func (s *Server) StopProcess(name string) error {
	s.logger.Info("Stopping process", "process", name)

	s.processMutex.Lock()
	defer s.processMutex.Unlock()

	proc, exists := s.processes[name]
	if !exists {
		s.logger.Warn("Process not found", "process", name)
		return fmt.Errorf("process %s not found", name)
	}

	currentState := proc.GetState()
	s.logger.Info("Current process state", "process", name, "state", currentState, "pid", proc.GetPID())

	s.logger.Info("Calling Stop()", "process", name)
	if err := proc.Stop(); err != nil {
		s.logger.Error("Error stopping process", "process", name, "error", err)
		return err
	}

	s.logger.Info("Process stopped successfully", "process", name)
	return nil
}

// RestartProcess restarts a specific process
func (s *Server) RestartProcess(name string) error {
	s.logger.Info("Restarting process", "process", name)
	if err := s.StopProcess(name); err != nil {
		s.logger.Error("Error stopping process during restart", "process", name, "error", err)
		return err
	}
	s.logger.Info("Waiting 100ms before restarting", "process", name)
	time.Sleep(pollInterval)
	s.logger.Info("Starting after restart", "process", name)
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
}

// onDependencyStop is called when a dependency stops
func (s *Server) onDependencyStop(name string) {
	// This is handled in onProcessStateChange
}

// monitorProcesses monitors all processes
func (s *Server) monitorProcesses() {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Periodic health checks could be added here
		case <-s.stopChan:
			return
		}
	}
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
