package process

import (
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// reapBackstop bounds how long an exit can go unnoticed if a SIGCHLD is missed.
// Signals coalesce, so several children exiting at once can produce one
// notification; the reap loop drains everything it can each time, and this only
// covers the case where a signal is lost entirely.
const reapBackstop = time.Second

var (
	reaperOnce sync.Once
	reaper     *childReaper
)

// childReaper owns every wait4 in the daemon.
//
// Waiting is a property of the process, not of any one Process object:
// wait4(-1) takes whatever has exited, so a second waiter steals statuses from
// the first. os/exec's Wait is exactly such a second waiter, which is why
// nothing here calls cmd.Wait() and every exit arrives through this type.
//
// Reaping anything, rather than only what we started, is also what makes
// supavisor safe to run as PID 1: a grandchild orphaned by a wrapper script is
// reparented to us, and if nobody waits for it, it stays a zombie forever.
type childReaper struct {
	logger  *slog.Logger
	waiters map[int]chan syscall.WaitStatus
	mu      sync.Mutex
}

// reaperFor returns the daemon's reaper, starting it on first use.
//
// It is started lazily rather than wired in explicitly because there can only
// ever be one, and because a supervisor that forgot to start it would hang
// waiting for exits that never arrive.
func reaperFor(logger *slog.Logger) *childReaper {
	reaperOnce.Do(func() {
		reaper = &childReaper{
			logger:  logger.With("component", "reaper"),
			waiters: make(map[int]chan syscall.WaitStatus),
		}
		go reaper.run()
	})
	return reaper
}

// spawn starts a child and registers it in one step, so an exit cannot be
// reaped before we know who it belongs to. start does the fork, and returns the
// PID it produced.
func (r *childReaper) spawn(start func() (int, error)) (<-chan syscall.WaitStatus, error) {
	// Held across the fork: the reap loop takes the same lock before wait4, so
	// it cannot collect a status for a PID that is still being registered.
	r.mu.Lock()
	defer r.mu.Unlock()

	pid, err := start()
	if err != nil {
		return nil, err
	}

	// Buffered so the reaper never blocks on a caller that is not listening yet.
	exited := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = exited
	return exited, nil
}

func (r *childReaper) run() {
	// Buffered: a SIGCHLD arriving while we are already reaping must not be
	// dropped, or the exit it refers to waits for the backstop.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGCHLD)

	ticker := time.NewTicker(reapBackstop)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
		case <-ticker.C:
		}
		r.reapAll()
	}
}

// reapAll collects every child that has exited, dispatching each status to
// whoever started it. It drains rather than taking one, because signals
// coalesce and several children can exit between two notifications.
func (r *childReaper) reapAll() {
	for {
		r.mu.Lock()

		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 {
			// 0: children exist, none have exited. ECHILD: none at all.
			r.mu.Unlock()
			if err != nil && err != syscall.ECHILD {
				r.logger.Debug("wait4 failed", "error", err)
			}
			return
		}

		exited := r.waiters[pid]
		delete(r.waiters, pid)
		r.mu.Unlock()

		if exited == nil {
			// Nobody started this: an orphan reparented to us because we are
			// PID 1. Reaping it is the whole point; there is no one to tell.
			r.logger.Debug("Reaped an orphan", "pid", pid)
			continue
		}

		exited <- status
		close(exited)
	}
}

// signalExitBase is the shell convention for reporting a process that was
// killed rather than exiting on its own: 128 plus the signal number.
const signalExitBase = 128

// exitCodeOfStatus reports the exit code a wait status carries.
func exitCodeOfStatus(status syscall.WaitStatus) int {
	if status.Signaled() {
		return signalExitBase + int(status.Signal())
	}
	return status.ExitStatus()
}
