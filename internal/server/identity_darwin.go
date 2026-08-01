//go:build darwin

package server

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartToken returns an opaque identifier for the current run of pid.
//
// A PID on its own does not identify a process: the kernel reuses PIDs, so a
// PID recorded before a crash may belong to something entirely unrelated by the
// time supavisor starts again. Pairing it with the start time distinguishes the
// process we recorded from a later one that inherited its number.
func processStartToken(pid int) (string, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("failed to read process info for pid %d: %w", pid, err)
	}

	started := proc.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", started.Sec, started.Usec), nil
}

// bootID identifies the running boot. macOS reports process start times as
// absolute wall clock, so this is not needed to tell one boot's PIDs from
// another's, but recording it keeps the state file's meaning the same on both
// platforms.
func bootID() (string, error) {
	booted, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", fmt.Errorf("failed to read boot time: %w", err)
	}
	return fmt.Sprintf("%d.%06d", booted.Sec, booted.Usec), nil
}
