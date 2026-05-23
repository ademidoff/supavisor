package supavisor

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

// ProcessStatusInfo contains status information about a process
type ProcessStatusInfo struct {
	Name         string
	State        process.State
	PID          int
	ExitCode     int
	RestartCount int
	Uptime       string
}

// Supavisor manages all processes
type Supavisor struct {
	config          *config.Config
	logger          *slog.Logger
	processLogger   *slog.Logger
	processes       map[string]*process.Process
	processMutex    sync.RWMutex
	dependencyGraph *dependency.Graph
	ipcServer       *IPCServer
	stopChan        chan struct{}
	running         bool
}

// NewSupavisor creates a new supavisor instance
func NewSupavisor(cfg *config.Config, logger *slog.Logger) (*Supavisor, error) {
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

	return &Supavisor{
		config:          cfg,
		logger:          logger.With("component", "main"),
		processLogger:   logger,
		processes:       make(map[string]*process.Process),
		dependencyGraph: graph,
		stopChan:        make(chan struct{}),
	}, nil
}

// Start starts the supavisor
func (s *Supavisor) Start() error {
	if s.running {
		return fmt.Errorf("supavisor is already running")
	}

	// Check if another instance is already running
	if err := s.checkIfRunning(); err != nil {
		return err
	}

	s.running = true

	// Start IPC server
	s.ipcServer = NewIPCServer(s.config.Supavisor.Socket, s)
	if err := s.ipcServer.Start(); err != nil {
		return fmt.Errorf("failed to start IPC server: %w", err)
	}

	// Write PID file
	if err := s.writePIDFile(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Setup signal handling
	s.setupSignalHandling()

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
func (s *Supavisor) Stop() error {
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
		_ = proc.Stop()
	}
	s.processMutex.Unlock()

	// Stop IPC server
	if s.ipcServer != nil {
		s.logger.Info("Stopping IPC server")
		s.ipcServer.Stop()
	}

	// Remove PID file
	s.removePIDFile()

	s.logger.Info("Supavisor daemon stopped")
	return nil
}

// startAutostartProcesses starts all processes configured to autostart
func (s *Supavisor) startAutostartProcesses() {
	// Get topological sort order
	order, err := s.dependencyGraph.TopologicalSort()
	if err != nil {
		s.logger.Warn("Failed to get startup order", "error", err)
		// Start processes in config order
		for name, progConfig := range s.config.Programs {
			if progConfig.Autostart {
				s.StartProcess(name)
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
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

// waitForSingleDependency waits for a single dependency to be running
func (s *Supavisor) waitForSingleDependency(dep string) error {
	// Wait up to 30 seconds for the dependency to be running
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
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
func (s *Supavisor) StartProcess(name string) error { //nolint:gocyclo
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
		proc.Stop()
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

	// Create and start process
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
func (s *Supavisor) StopProcess(name string) error {
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
func (s *Supavisor) RestartProcess(name string) error {
	s.logger.Info("Restarting process", "process", name)
	if err := s.StopProcess(name); err != nil {
		s.logger.Error("Error stopping process during restart", "process", name, "error", err)
		return err
	}
	s.logger.Info("Waiting 100ms before restarting", "process", name)
	time.Sleep(100 * time.Millisecond)
	s.logger.Info("Starting after restart", "process", name)
	return s.StartProcess(name)
}

// Reload reloads the configuration
func (s *Supavisor) Reload() error {
	// For now, just validate the current config
	// Full reload would require stopping and restarting processes
	return s.config.Validate()
}

// GetStatus returns the status of all processes
func (s *Supavisor) GetStatus() []ProcessStatusInfo {
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
func (s *Supavisor) onProcessStateChange(name string, prevState, newState process.State) {
	if prevState != newState {
		s.logger.Info("Process state changed", "process", name, "prev_state", prevState, "new_state", newState)
	}
}

// onDependencyStop is called when a dependency stops
func (s *Supavisor) onDependencyStop(name string) {
	// This is handled in onProcessStateChange
}

// monitorProcesses monitors all processes
func (s *Supavisor) monitorProcesses() {
	ticker := time.NewTicker(5 * time.Second)
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
func (s *Supavisor) setupSignalHandling() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		s.logger.Info("Received signal to stop supavisor", "signal", sig.String())
		s.Stop()
		os.Exit(0)
	}()
}

// checkIfRunning checks if another instance is already running
func (s *Supavisor) checkIfRunning() error {
	// Check if PID file exists and process is running
	if s.config.Supavisor.PidFile != "" {
		if data, err := os.ReadFile(s.config.Supavisor.PidFile); err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(data), "%d", &pid); err == nil {
				// Check if process is still running
				if proc, err := os.FindProcess(pid); err == nil {
					// Send signal 0 to check if process exists
					if err := proc.Signal(syscall.Signal(0)); err == nil {
						return fmt.Errorf("supavisor is already running (PID: %d)", pid)
					}
				}
			}
			// PID file exists but process is not running - this is a stale file
			return fmt.Errorf("found stale PID file: %s\nThe previous instance may not have exited cleanly. Please remove it manually and check the logs", //nolint:lll
				s.config.Supavisor.PidFile)
		}
	}

	// Check if socket is in use
	if s.config.Supavisor.Socket != "" {
		if _, err := os.Stat(s.config.Supavisor.Socket); err == nil {
			// Try to connect to see if it's actually in use
			conn, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err == nil {
				defer syscall.Close(conn)
				addr := &syscall.SockaddrUnix{Name: s.config.Supavisor.Socket}
				if err := syscall.Connect(conn, addr); err == nil {
					return fmt.Errorf("supavisor socket is already in use: %s", s.config.Supavisor.Socket)
				}
			}
			// Socket file exists but not in use - this is a stale file
			return fmt.Errorf("found stale socket file: %s\nThe previous instance may not have exited cleanly. Please remove it manually and check the logs", //nolint:lll
				s.config.Supavisor.Socket)
		}
	}

	return nil
}

// writePIDFile writes the PID file
func (s *Supavisor) writePIDFile() error {
	if s.config.Supavisor.PidFile == "" {
		return nil
	}

	pid := os.Getpid()
	return os.WriteFile(s.config.Supavisor.PidFile, fmt.Appendf(nil, "%d\n", pid), 0o644)
}

// removePIDFile removes the PID file
func (s *Supavisor) removePIDFile() {
	if s.config.Supavisor.PidFile != "" {
		os.Remove(s.config.Supavisor.PidFile)
	}
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
