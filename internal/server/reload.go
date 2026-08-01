package server

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/dependency"
	"github.com/ademidoff/supavisor/internal/process"
)

// Reload re-reads the configuration files and applies what changed.
//
// Programs that are unchanged keep running untouched. Programs that were
// removed are stopped and forgotten, programs that were added are picked up by
// the reconciler, and programs whose definition changed are stopped and
// replaced so they come back on the new definition.
func (s *Server) Reload() error {
	// One reload at a time: two of them interleaving would compute their plans
	// against the same starting point and then apply both.
	s.reloadMutex.Lock()
	defer s.reloadMutex.Unlock()

	if s.config.SourcePath == "" {
		return fmt.Errorf("no configuration file to reload")
	}

	newCfg, err := config.ParseConfig(s.config.SourcePath)
	if err != nil {
		return fmt.Errorf("failed to reload configuration: %w", err)
	}
	if err := newCfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if _, err := buildDependencyGraph(newCfg).TopologicalSort(); err != nil {
		return fmt.Errorf("dependency graph validation failed: %w", err)
	}
	if setting := changedDaemonSetting(&s.config.Supavisor, &newCfg.Supavisor); setting != "" {
		return fmt.Errorf("%s cannot be changed while running: restart supavisor to apply it", setting)
	}
	if err := newCfg.EnsureLogDirectories(); err != nil {
		return fmt.Errorf("failed to create log directories: %w", err)
	}

	added, removed, changed := diffPrograms(s.config.Programs, newCfg.Programs)
	if len(added)+len(removed)+len(changed) == 0 {
		s.logger.Info("Configuration reloaded, nothing changed")
		return nil
	}
	s.logger.Info("Reloading configuration", "added", added, "removed", removed, "changed", changed)

	// Whatever an operator asked for survives the reload: a program that was
	// deliberately stopped must not come back just because its definition moved.
	s.processMutex.RLock()
	previous := maps.Clone(s.desired)
	s.processMutex.RUnlock()

	// A program that is going away, or is about to be redefined, has to stop
	// before it can be dropped or replaced.
	s.stopForReload(slices.Concat(removed, changed))

	s.processMutex.Lock()
	s.applyPrograms(newCfg, previous, added, removed, changed)
	s.processMutex.Unlock()

	s.requestReconcile()
	s.markStateDirty()

	s.logger.Info("Configuration reloaded")
	return nil
}

// stopForReload stops programs that reload is about to drop or replace
func (s *Server) stopForReload(names []string) {
	var wg sync.WaitGroup

	for _, name := range names {
		if err := s.setDesired(name, DesiredStopped); err != nil {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.awaitState(name, func(state process.State) bool {
				return state == process.StateStopped
			})
			if err != nil {
				s.logger.Warn("failed to stop process for reload", "process", name, "error", err)
			}
		}()
	}

	s.requestReconcile()
	wg.Wait()
}

// applyPrograms swaps in the new configuration. Must be called with
// processMutex held.
func (s *Server) applyPrograms(newCfg *config.Config, previous map[string]DesiredState, added, removed, changed []string) {
	for _, name := range removed {
		delete(s.processes, name)
		delete(s.desired, name)
		delete(s.inflight, name)
	}

	for _, name := range changed {
		s.processes[name] = s.newProcess(newCfg.Programs[name])
		s.desired[name] = previous[name]
	}

	for _, name := range added {
		s.processes[name] = s.newProcess(newCfg.Programs[name])
		s.desired[name] = DesiredStopped
		if newCfg.Programs[name].Autostart {
			s.desired[name] = DesiredRunning
		}
	}

	s.config = newCfg
	s.dependencyGraph = buildDependencyGraph(newCfg)
}

// diffPrograms reports which programs a new configuration adds, drops and
// redefines
func diffPrograms(old, updated map[string]*config.ProgramConfig) (added, removed, changed []string) {
	for name, newProg := range updated {
		oldProg, exists := old[name]
		switch {
		case !exists:
			added = append(added, name)
		case !reflect.DeepEqual(oldProg, newProg):
			changed = append(changed, name)
		}
	}

	for name := range old {
		if _, exists := updated[name]; !exists {
			removed = append(removed, name)
		}
	}

	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return added, removed, changed
}

// changedDaemonSetting names a daemon-level setting that differs, or an empty
// string if they match. These are bound at startup: the PID file is locked and
// the socket is already listening, so changing them means restarting.
func changedDaemonSetting(old, updated *config.SupavisorConfig) string {
	switch {
	case old.PidFile != updated.PidFile:
		return "pidfile"
	case old.Socket != updated.Socket:
		return "socket"
	case old.SocketGroup != updated.SocketGroup:
		return "socket_group"
	case old.LogFile != updated.LogFile:
		return "logfile"
	case old.LogFormat != updated.LogFormat:
		return "log_format"
	case old.LogLevel != updated.LogLevel:
		return "log_level"
	}
	return ""
}

// buildDependencyGraph builds the dependency graph for a configuration
func buildDependencyGraph(cfg *config.Config) *dependency.Graph {
	graph := dependency.NewGraph()
	for name, progConfig := range cfg.Programs {
		graph.AddNode(name, progConfig.DependsOn)
	}
	return graph
}

// newProcess creates a managed process wired to this server's callbacks
func (s *Server) newProcess(cfg *config.ProgramConfig) *process.Process {
	proc := process.NewProcess(cfg, s.processLogger)
	proc.SetStateChangeCallback(s.onProcessStateChange)
	return proc
}
