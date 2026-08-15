package process

import (
	"errors"
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

	// forking excludes reaping, so a status cannot be collected for a PID that
	// a start has not registered yet. It is read-held across the fork, which
	// lets starts run in parallel with each other: holding one exclusive lock
	// across every fork made a slow exec stall every other program's start and
	// all exit delivery with it.
	forking sync.RWMutex

	// mu guards waiters alone, and is never held across a fork or a syscall
	// that can block.
	mu sync.Mutex
}

// StartReaping starts the daemon's reaper. Call it once, from daemon startup,
// with the root logger.
//
// Doing it here rather than on the first fork is what makes the PID 1 guarantee
// unconditional: a container whose programs are all autostart: false, or one
// still working through dependencies, would otherwise have no wait4 loop at all
// and would accumulate any orphan reparented onto it in the meantime.
func StartReaping(logger *slog.Logger) {
	reaperFor(logger)
}

// reaperFor returns the daemon's reaper, starting it if StartReaping has not
// run. The fallback keeps a Process usable on its own, in tests and anywhere
// else that builds one directly, at the cost of the reaper inheriting that
// caller's logger.
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
	// Read-held across the fork: reapAll takes it exclusively, so it cannot
	// collect a status for a PID that is still being registered, while other
	// starts are free to proceed alongside this one.
	r.forking.RLock()
	defer r.forking.RUnlock()

	pid, err := start()
	if err != nil {
		return nil, err
	}

	// Buffered so the reaper never blocks on a caller that is not listening yet.
	exited := make(chan syscall.WaitStatus, 1)

	r.mu.Lock()
	defer r.mu.Unlock()
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
	// No fork may be in flight while statuses are being collected.
	r.forking.Lock()
	defer r.forking.Unlock()

	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 {
			// 0: children exist, none have exited. ECHILD: none at all.
			if err != nil && !errors.Is(err, syscall.ECHILD) {
				r.logger.Debug("wait4 failed", "error", err)
			}
			return
		}

		r.mu.Lock()
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

// signalGroupIfUnreaped signals pid's process group, but only while the reaper
// still holds that PID. Once wait4 has collected it the kernel is free to hand
// the number to something else, and kill(-pid) would then reach an unrelated
// process group. Held under the same lock reapAll uses to drop the waiter, so
// the check cannot go stale between here and the signal.
func (r *childReaper) signalGroupIfUnreaped(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, waiting := r.waiters[pid]; !waiting {
		return nil
	}
	return SignalGroup(pid, sig)
}

// exitCodeSignalled is what a process killed by a signal reports, matching
// os.ProcessState.ExitCode.
const exitCodeSignalled = -1

// exitCodeOfStatus reports the exit code a wait status carries.
//
// A signaled process reports -1 rather than 128+signal. That is what
// os.ProcessState.ExitCode gives, which is what this used to be built on, and
// it reaches operators through the exit_code field of the status API. The shell
// convention would be more informative, but changing a documented field is a
// deliberate act rather than a side effect of moving where the status is read.
func exitCodeOfStatus(status syscall.WaitStatus) int {
	if status.Signaled() {
		return exitCodeSignalled
	}
	return status.ExitStatus()
}
