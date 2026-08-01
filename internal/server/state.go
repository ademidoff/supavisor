package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ademidoff/supavisor/internal/process"
)

const (
	// orphanStopTimeout is how long an orphan gets to exit on SIGTERM before it
	// is killed.
	orphanStopTimeout = 5 * time.Second

	// orphanPollInterval is how often orphans are checked for exit during that
	// window.
	orphanPollInterval = 100 * time.Millisecond
)

// childRecord identifies one managed process well enough for a later daemon to
// decide whether the process now holding that PID is the one we started.
type childRecord struct {
	Name       string `json:"name"`
	StartToken string `json:"start_token"`
	PID        int    `json:"pid"`
}

// stateFilePath returns the sibling state file for a given pid file, so that
// /var/run/supavisor.pid is tracked by /var/run/supavisor.state.
func stateFilePath(pidFile string) string {
	if pidFile == "" {
		return ""
	}
	return strings.TrimSuffix(pidFile, filepath.Ext(pidFile)) + ".state"
}

// writeStateFile replaces the state file atomically, so a crash mid-write can
// never leave a half-written record behind for the next daemon to act on.
func writeStateFile(path string, records []childRecord) error {
	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("failed to encode process state: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("failed to write process state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to replace process state: %w", err)
	}
	return nil
}

// readStateFile returns the processes recorded by a previous daemon
func readStateFile(path string) ([]childRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read process state: %w", err)
	}

	var records []childRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("failed to decode process state %s: %w", path, err)
	}
	return records, nil
}

// saveState records the processes supavisor currently owns, so that a daemon
// starting after a crash can recognize what outlived it
func (s *Server) saveState() {
	if s.stateFile == "" {
		return
	}

	s.processMutex.RLock()
	records := make([]childRecord, 0, len(s.processes))
	for name, proc := range s.processes {
		pid := proc.GetPID()
		if pid <= 0 || proc.GetState().IsStopped() {
			continue
		}
		token, err := processStartToken(pid)
		if err != nil {
			continue
		}
		records = append(records, childRecord{Name: name, PID: pid, StartToken: token})
	}
	s.processMutex.RUnlock()

	if err := writeStateFile(s.stateFile, records); err != nil {
		s.logger.Warn("failed to record process state", "error", err)
	}
}

// markStateDirty asks for the state file to be refreshed.
//
// This is called from process state change callbacks, which run while the
// caller may hold processMutex for writing, so the write has to happen on
// another goroutine rather than inline.
func (s *Server) markStateDirty() {
	select {
	case s.stateDirty <- struct{}{}:
	default:
	}
}

// recordStateChanges rewrites the state file whenever processes change,
// coalescing bursts into a single write
func (s *Server) recordStateChanges() {
	for {
		select {
		case <-s.stateDirty:
			s.saveState()
		case <-s.stopChan:
			return
		}
	}
}

// reapOrphans stops processes left running by a previous daemon that did not
// shut down cleanly. Supavisor cannot adopt them: they are not its children, so
// it can neither wait on them nor learn how they exit.
func (s *Server) reapOrphans() {
	if s.stateFile == "" {
		return
	}

	records, err := readStateFile(s.stateFile)
	if err != nil {
		s.logger.Warn("failed to read previous process state", "error", err)
		return
	}
	if len(records) == 0 {
		return
	}

	orphans := s.identifyOrphans(records)
	if len(orphans) == 0 {
		s.clearStateFile()
		return
	}

	for _, orphan := range orphans {
		s.logger.Warn("Stopping a process left behind by a previous supavisor",
			"process", orphan.Name, "pid", orphan.PID)
		if err := process.SignalGroup(orphan.PID, syscall.SIGTERM); err != nil {
			s.logger.Warn("failed to signal orphan", "process", orphan.Name, "pid", orphan.PID, "error", err)
		}
	}

	s.waitForOrphans(orphans)
	s.clearStateFile()
}

// identifyOrphans filters recorded processes down to those still running under
// the same identity
func (s *Server) identifyOrphans(records []childRecord) []childRecord {
	orphans := make([]childRecord, 0, len(records))

	for _, rec := range records {
		token, err := processStartToken(rec.PID)
		if err != nil {
			// Either the process is gone, or this platform cannot prove
			// identity, in which case killing by PID alone is not safe.
			continue
		}
		if token != rec.StartToken {
			s.logger.Info("Recorded pid now belongs to an unrelated process, leaving it alone",
				"process", rec.Name, "pid", rec.PID)
			continue
		}
		orphans = append(orphans, rec)
	}

	return orphans
}

// waitForOrphans gives orphans a chance to exit, then kills what is left
func (s *Server) waitForOrphans(orphans []childRecord) {
	deadline := time.Now().Add(orphanStopTimeout)

	for time.Now().Before(deadline) {
		if !anyGroupAlive(orphans) {
			s.logger.Info("Orphaned processes exited")
			return
		}
		time.Sleep(orphanPollInterval)
	}

	for _, orphan := range orphans {
		if err := process.SignalGroup(orphan.PID, syscall.Signal(0)); err != nil {
			continue
		}
		s.logger.Warn("Orphan did not exit, killing it", "process", orphan.Name, "pid", orphan.PID)
		if err := process.SignalGroup(orphan.PID, syscall.SIGKILL); err != nil {
			s.logger.Warn("failed to kill orphan", "process", orphan.Name, "pid", orphan.PID, "error", err)
		}
	}
}

func anyGroupAlive(records []childRecord) bool {
	for _, rec := range records {
		if err := process.SignalGroup(rec.PID, syscall.Signal(0)); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) clearStateFile() {
	if err := os.Remove(s.stateFile); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("failed to remove process state file", "error", err)
	}
}
