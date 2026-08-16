package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/process"
)

// DesiredState is what supavisor has been asked to keep a program at, which is
// separate from what the program happens to be doing right now.
type DesiredState string

const (
	DesiredRunning DesiredState = "RUNNING"
	DesiredStopped DesiredState = "STOPPED"
)

// reconcileInterval is the slowest supavisor will notice that a program has
// drifted from its desired state. Most of the time it does not wait for the
// tick: every process state change asks for a pass immediately.
const reconcileInterval = time.Second

// reconcileLoop drives programs towards their desired state until shutdown.
//
// This exists because starting a program is not a one-shot action: a program
// whose dependencies were not ready yet has to be started later, when they are.
// Without a loop that keeps looking, one dependency that was slow to come up
// left everything behind it stopped for good.
func (s *Server) reconcileLoop() {
	defer close(s.reconcileDone)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		s.reconcile()

		select {
		case <-s.reconcileNow:
		case <-ticker.C:
		case <-s.stopChan:
			return
		}
	}
}

// requestReconcile asks for a pass without waiting for the next tick. It never
// blocks, so it is safe to call from process state change callbacks.
func (s *Server) requestReconcile() {
	select {
	case s.reconcileNow <- struct{}{}:
	default:
	}
}

// reconcile moves every program one step towards its desired state.
//
// It does no work itself: anything that can take time runs on its own
// goroutine, so a program that takes seconds to stop does not hold up the rest.
func (s *Server) reconcile() {
	for _, name := range s.programNames() {
		s.processMutex.RLock()
		desired := s.desired[name]
		proc := s.processes[name]
		busy := s.inflight[name]
		s.processMutex.RUnlock()

		if busy || proc == nil {
			continue
		}

		state := proc.GetState()

		switch {
		// Only STOPPED is started. A process that exited or gave up on its own
		// is left alone: restarts within a run are the autorestart policy's
		// business, and starting a FATAL process here would defeat
		// max_restarts entirely.
		case desired == DesiredRunning && state == process.StateStopped:
			ready, reason := s.dependenciesSatisfied(name)
			if !ready {
				s.noteBlocked(name, reason)
				continue
			}
			s.noteBlocked(name, "")
			s.logger.Info("Starting process", "process", name)
			s.runAction(name, proc.Start)

		// A program that has exited or given up is not running, but it is not
		// released either: its monitor may be sitting in a restart backoff that
		// only Stop() cancels. Leaving it alone let a stopped program spawn a
		// run nobody was asking for, one backoff after the stop was reported
		// as done.
		case desired == DesiredStopped && state != process.StateStopped && state != process.StateStopping:
			s.logger.Info("Stopping process", "process", name)
			s.runAction(name, proc.Stop)
		}
	}
}

// noteBlocked reports why a program cannot start yet, once per distinct reason.
// An empty reason forgets what was last reported, so that a program blocked
// again later says so again.
//
// The reconciler runs every second and a dependency can be down for a long
// time, so reporting every pass would bury everything else in the log; saying
// nothing left a program that never starts with no explanation at all.
func (s *Server) noteBlocked(name, reason string) {
	s.processMutex.Lock()
	changed := s.blockedReason[name] != reason
	if reason == "" {
		delete(s.blockedReason, name)
	} else {
		s.blockedReason[name] = reason
	}
	s.processMutex.Unlock()

	if changed && reason != "" {
		s.logger.Info("Not starting yet", "process", name, "reason", reason)
	}
}

// runAction performs a transition off the reconcile loop and marks the program
// busy so that the next pass does not start a second one
func (s *Server) runAction(name string, action func() error) {
	s.processMutex.Lock()
	s.inflight[name] = true
	s.processMutex.Unlock()

	s.actions.Add(1)
	go func() {
		defer s.actions.Done()

		if err := action(); err != nil {
			s.logger.Error("Failed to reconcile process", "process", name, "error", err)
		}

		s.processMutex.Lock()
		delete(s.inflight, name)
		s.processMutex.Unlock()

		s.requestReconcile()
	}()
}

// dependenciesSatisfied reports whether every program this one depends on has
// reached the condition it is waited on for, and if not, which one is holding
// it back
func (s *Server) dependenciesSatisfied(name string) (satisfied bool, reason string) {
	for _, dep := range s.dependenciesOf(name) {
		s.processMutex.RLock()
		depProc, exists := s.processes[dep.Name]
		s.processMutex.RUnlock()

		if !exists {
			return false, fmt.Sprintf("dependency %s is not configured", dep.Name)
		}

		// A one-off is waited on for having finished rather than for being up,
		// so RUNNING is the wrong thing to ask of it: it is only running while
		// the work is still in flight.
		if dep.Condition == config.ConditionCompleted {
			if !depProc.HasCompleted() {
				return false, fmt.Sprintf("dependency %s has not completed, it is %s", dep.Name, depProc.GetState())
			}
			continue
		}

		if state := depProc.GetState(); state != process.StateRunning {
			return false, fmt.Sprintf("dependency %s is %s", dep.Name, state)
		}
		// Running only means the process is alive. A program that initializes
		// after startup is not ready to be depended on until its check passes.
		if dep.Condition == config.ConditionHealthy {
			if health := depProc.GetHealth(); health != process.HealthHealthy {
				return false, fmt.Sprintf("dependency %s is running but its health check is %s", dep.Name, health)
			}
		}
	}
	return true, ""
}

// dependenciesOf returns what a program depends on, with the condition each
// dependency has to reach. The graph carries the ordering, the configuration
// carries the conditions.
func (s *Server) dependenciesOf(name string) []config.Dependency {
	s.processMutex.RLock()
	defer s.processMutex.RUnlock()

	prog := s.config.Programs[name]
	if prog == nil {
		return nil
	}
	return prog.DependsOn
}

// programNames returns configured program names ordered by priority, lowest
// first, with names breaking ties so the order is stable.
//
// Dependencies still decide what may start; priority only settles the order
// among programs that are all ready at the same moment.
func (s *Server) programNames() []string {
	s.processMutex.RLock()
	defer s.processMutex.RUnlock()

	names := make([]string, 0, len(s.processes))
	for name := range s.processes {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		a, b := s.config.Programs[names[i]], s.config.Programs[names[j]]
		if a != nil && b != nil && a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return names[i] < names[j]
	})
	return names
}

// process returns a managed process by name
func (s *Server) process(name string) *process.Process {
	s.processMutex.RLock()
	defer s.processMutex.RUnlock()
	return s.processes[name]
}

// setDesired records what a program should be doing
func (s *Server) setDesired(name string, desired DesiredState) error {
	s.processMutex.Lock()
	defer s.processMutex.Unlock()

	if _, exists := s.processes[name]; !exists {
		return fmt.Errorf("process %s not found", name)
	}
	s.desired[name] = desired

	// Nothing is holding back a program nobody is asking for, so a reason
	// recorded while it was wanted must not outlive the request.
	if desired == DesiredStopped {
		delete(s.blockedReason, name)
	}
	return nil
}

// awaitState waits for the reconciler to bring a program to a settled state, so
// that a caller still learns what actually happened rather than only that the
// request was recorded.
func (s *Server) awaitState(name string, done func(process.State) bool) error {
	proc := s.process(name)
	if proc == nil {
		return fmt.Errorf("process %s not found", name)
	}

	deadline := time.After(actionTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if done(proc.GetState()) {
			return nil
		}
		// Waiting out the timeout is only worth it while the outcome can still
		// change. If a dependency is not even meant to come up, say so now.
		//
		// Only for a program that is wanted running: dependencies decide what
		// may start, never what may stop, and a stop that was proceeding
		// perfectly well used to report that the program could not start.
		if s.wantsToRun(name) {
			if blocked, reason := s.blockedIndefinitely(name); blocked {
				return fmt.Errorf("process %s cannot start: %s", name, reason)
			}
		}

		select {
		case <-ticker.C:
		case <-s.stopChan:
			return fmt.Errorf("supavisor is shutting down")
		case <-deadline:
			return s.awaitTimeoutError(name, proc.GetState())
		}
	}
}

// wantsToRun reports whether a program is currently meant to be running
func (s *Server) wantsToRun(name string) bool {
	s.processMutex.RLock()
	defer s.processMutex.RUnlock()
	return s.desired[name] == DesiredRunning
}

// blockedIndefinitely reports a dependency that is not merely down but has no
// prospect of coming up, so that waiting for it would never resolve.
//
// The walk is transitive: a dependency that is itself waiting on something that
// will never start is just as final as one that is stopped outright.
func (s *Server) blockedIndefinitely(name string) (blocked bool, reason string) {
	return s.blockedByDependency(name, make(map[string]bool))
}

func (s *Server) blockedByDependency(name string, seen map[string]bool) (blocked bool, reason string) {
	if seen[name] {
		return false, ""
	}
	seen[name] = true

	for _, dep := range s.dependenciesOf(name) {
		s.processMutex.RLock()
		depProc := s.processes[dep.Name]
		depDesired := s.desired[dep.Name]
		s.processMutex.RUnlock()

		if depProc == nil {
			return true, fmt.Sprintf("dependency %s is not configured", dep.Name)
		}

		state := depProc.GetState()
		waitsForCompletion := dep.Condition == config.ConditionCompleted

		switch {
		// Work that is done stays done: what the program is left sitting in
		// afterwards, and whether anyone means to run it again, no longer
		// decides whether a dependent may start.
		case waitsForCompletion && depProc.HasCompleted():
			continue
		case state == process.StateFatal:
			return true, fmt.Sprintf("dependency %s gave up starting", dep.Name)
		case depDesired == DesiredStopped && state.IsStopped():
			return true, fmt.Sprintf("dependency %s is stopped and is not set to start", dep.Name)
		case waitsForCompletion && s.exitedForGood(dep.Name, state):
			return true, fmt.Sprintf("dependency %s exited with status %d and will not be run again",
				dep.Name, depProc.GetExitCode())
		case state == process.StateRunning:
			continue
		}

		if blockedDeep, deepReason := s.blockedByDependency(dep.Name, seen); blockedDeep {
			return true, deepReason
		}
	}

	return false, ""
}

// exitedForGood reports a program that has exited and whose restart policy
// declined to run it again.
//
// Unlike a FATAL program, which gave up on its way somewhere, this one has
// settled: waiting for it to complete would never resolve. Only autorestart:
// never can leave it here, since the other policies either restart an
// unsuccessful exit or eventually reach FATAL.
func (s *Server) exitedForGood(name string, state process.State) bool {
	if state != process.StateExited {
		return false
	}

	s.processMutex.RLock()
	defer s.processMutex.RUnlock()
	return s.autorestartPolicy(name) == config.RestartNever
}

// awaitTimeoutError explains why a program never reached the expected state
func (s *Server) awaitTimeoutError(name string, state process.State) error {
	if ready, reason := s.dependenciesSatisfied(name); !ready {
		return fmt.Errorf("process %s is not running: %s", name, reason)
	}
	return fmt.Errorf("timed out waiting for process %s, it is %s", name, state)
}
