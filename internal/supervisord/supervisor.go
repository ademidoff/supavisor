package supervisord

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ademidoff/go-supervisord/internal/config"
	"github.com/ademidoff/go-supervisord/internal/dependency"
	"github.com/ademidoff/go-supervisord/internal/process"
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

// supervisord manages all processes
type Supervisord struct {
	config          *config.Config
	processes       map[string]*process.Process
	processMutex    sync.RWMutex
	dependencyGraph *dependency.Graph
	ipcServer       *IPCServer
	stopChan        chan struct{}
	running         bool
}

// NewSupervisor creates a new supervisord instance
func NewSupervisor(cfg *config.Config) (*Supervisord, error) {
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

	return &Supervisord{
		config:          cfg,
		processes:       make(map[string]*process.Process),
		dependencyGraph: graph,
		stopChan:        make(chan struct{}),
	}, nil
}

// Start starts the supervisord
func (s *Supervisord) Start() error {
	if s.running {
		return fmt.Errorf("supervisord is already running")
	}

	s.running = true

	// Start IPC server
	s.ipcServer = NewIPCServer(s.config.Supervisord.Socket, s)
	if err := s.ipcServer.Start(); err != nil {
		return fmt.Errorf("failed to start IPC server: %w", err)
	}

	// Write PID file
	if err := s.writePIDFile(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Setup signal handling
	s.setupSignalHandling()

	log.Println("Starting supervisord daemon")
	log.Printf("IPC server started on socket: %s\n", s.config.Supervisord.Socket)

	// Start processes that should autostart
	s.startAutostartProcesses()

	// Monitor processes
	go s.monitorProcesses()

	log.Println("Supervisord daemon started successfully")
	return nil
}

// Stop stops the supervisord and all processes
func (s *Supervisord) Stop() error {
	if !s.running {
		return nil
	}

	log.Println("Stopping supervisord daemon")
	s.running = false
	close(s.stopChan)

	// Stop all processes
	s.processMutex.Lock()
	processCount := len(s.processes)
	log.Printf("Stopping %d process(es)\n", processCount)
	for name, proc := range s.processes {
		log.Printf("Stopping process: %s\n", name)
		if err := proc.Stop(); err != nil {
			// Log error but continue
			log.Printf("Error stopping process %s: %v\n", name, err)
		} else {
			log.Printf("Process %s stopped successfully\n", name)
		}
	}
	s.processMutex.Unlock()

	// Stop IPC server
	if s.ipcServer != nil {
		log.Println("Stopping IPC server")
		s.ipcServer.Stop()
	}

	// Remove PID file
	s.removePIDFile()

	log.Println("Supervisord daemon stopped")
	return nil
}

// startAutostartProcesses starts all processes configured to autostart
func (s *Supervisord) startAutostartProcesses() {
	// Get topological sort order
	order, err := s.dependencyGraph.TopologicalSort()
	if err != nil {
		log.Printf("Warning: failed to get startup order: %v\n", err)
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
			log.Printf("Starting process: %s (autostart enabled)\n", name)
			if err := s.StartProcess(name); err != nil {
				log.Printf("Failed to start process %s: %v\n", name, err)
			} else {
				log.Printf("Process %s started successfully\n", name)
				// Give the process a moment to transition from STARTING to RUNNING
				// This helps dependent processes that check immediately
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

// waitForSingleDependency waits for a single dependency to be running
func (s *Supervisord) waitForSingleDependency(dep string) error {
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
func (s *Supervisord) StartProcess(name string) error {
	progConfig, exists := s.config.Programs[name]
	if !exists {
		return fmt.Errorf("process %s not found", name)
	}

	log.Printf("Starting process: %s\n", name)

	s.processMutex.Lock()
	defer s.processMutex.Unlock()

	// Check if already exists
	if _, exists := s.processes[name]; exists {
		// Process already exists, check if it's running
		proc := s.processes[name]
		if proc.GetState() == process.StateRunning {
			log.Printf("Process %s is already running\n", name)
			return fmt.Errorf("process %s is already running", name)
		}
		// Stop existing process if needed
		log.Printf("Stopping existing process %s before restart\n", name)
		proc.Stop()
	}

	// Check dependencies and wait for them to be running if they're starting
	deps := s.dependencyGraph.GetDependencies(name)
	if len(deps) > 0 {
		log.Printf("%s: has %d dependencies: %v\n", name, len(deps), deps)
	}
	for _, dep := range deps {
		depProc, exists := s.processes[dep]
		if !exists {
			// Dependency doesn't exist yet, wait for it to be created and running
			log.Printf("%s: waiting for dependency %s to start...\n", name, dep)
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
			log.Printf("Dependency %s is now running\n", dep)
			continue
		}
		state := depProc.GetState()
		if state == process.StateStarting {
			log.Printf("%s: waiting for dependency %s to finish starting...\n", name, dep)
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
			log.Printf("%s: dependency %s is now running\n", name, dep)
		} else if state != process.StateRunning {
			return fmt.Errorf("dependency %s is not running (state: %s)", dep, state)
		}
	}

	// Create and start process
	log.Printf("%s: creating process instance\n", name)
	proc := process.NewProcess(progConfig)
	proc.SetStateChangeCallback(s.onProcessStateChange)
	proc.SetDependencyStopCallback(s.onDependencyStop)

	log.Printf("%s: calling Start()\n", name)
	if err := proc.Start(); err != nil {
		log.Printf("%s: failed to start: %v\n", name, err)
		return fmt.Errorf("failed to start process: %w", err)
	}

	s.processes[name] = proc
	log.Printf("%s: started (PID: %d)\n", name, proc.GetPID())
	return nil
}

// StopProcess stops a specific process
func (s *Supervisord) StopProcess(name string) error {
	log.Printf("%s: stopping\n", name)

	s.processMutex.Lock()
	defer s.processMutex.Unlock()

	proc, exists := s.processes[name]
	if !exists {
		log.Printf("%s: not found\n", name)
		return fmt.Errorf("process %s not found", name)
	}

	currentState := proc.GetState()
	log.Printf("%s: current state: %s (PID: %d)\n", name, currentState, proc.GetPID())

	// Check if any processes depend on this one
	dependents := s.dependencyGraph.GetDependents(name)
	if len(dependents) > 0 {
		log.Printf("%s: has %d dependent process(es): %v\n", name, len(dependents), dependents)
		for _, dep := range dependents {
			depProc, exists := s.processes[dep]
			if exists && depProc.GetState() == process.StateRunning {
				// Stop dependent processes if configured
				depConfig := s.config.Programs[dep]
				if depConfig.StopOnDependencyFailure {
					log.Printf("%s: stopping dependent process %s (stop_on_dependency_failure=true)\n", name, dep)
					depProc.Stop()
				}
			}
		}
	}

	log.Printf("%s: calling Stop()\n", name)
	if err := proc.Stop(); err != nil {
		log.Printf("%s: error stopping: %v\n", name, err)
		return err
	}

	log.Printf("%s: stopped successfully\n", name)
	return nil
}

// RestartProcess restarts a specific process
func (s *Supervisord) RestartProcess(name string) error {
	log.Printf("%s: restarting\n", name)
	if err := s.StopProcess(name); err != nil {
		log.Printf("%s: error stopping during restart: %v\n", name, err)
		return err
	}
	log.Printf("%s: waiting 100ms before restarting\n", name)
	time.Sleep(100 * time.Millisecond)
	log.Printf("%s: starting after restart\n", name)
	return s.StartProcess(name)
}

// Reload reloads the configuration
func (s *Supervisord) Reload() error {
	// For now, just validate the current config
	// Full reload would require stopping and restarting processes
	return s.config.Validate()
}

// GetStatus returns the status of all processes
func (s *Supervisord) GetStatus() []ProcessStatusInfo {
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
func (s *Supervisord) onProcessStateChange(name string, oldState, newState process.State) {
	if oldState != newState {
		log.Printf("%s: state changed: %s -> %s\n", name, oldState, newState)
	}

	// Handle dependency failures
	if newState == process.StateExited || newState == process.StateFatal {
		log.Printf("%s: exited or failed, checking dependents\n", name)
		dependents := s.dependencyGraph.GetDependents(name)
		for _, dep := range dependents {
			depProc, exists := s.processes[dep]
			if exists {
				depConfig := s.config.Programs[dep]
				if depConfig.StopOnDependencyFailure {
					log.Printf("%s: stopping dependent process %s due to dependency %s failure\n", name, dep, name)
					depProc.Stop()
				}
			}
		}
	}
}

// onDependencyStop is called when a dependency stops
func (s *Supervisord) onDependencyStop(name string) {
	// This is handled in onProcessStateChange
}

// monitorProcesses monitors all processes
func (s *Supervisord) monitorProcesses() {
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
func (s *Supervisord) setupSignalHandling() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		s.Stop()
		os.Exit(0)
	}()
}

// writePIDFile writes the PID file
func (s *Supervisord) writePIDFile() error {
	if s.config.Supervisord.PidFile == "" {
		return nil
	}

	pid := os.Getpid()
	return os.WriteFile(s.config.Supervisord.PidFile, []byte(fmt.Sprintf("%d\n", pid)), 0644)
}

// removePIDFile removes the PID file
func (s *Supervisord) removePIDFile() {
	if s.config.Supervisord.PidFile != "" {
		os.Remove(s.config.Supervisord.PidFile)
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
