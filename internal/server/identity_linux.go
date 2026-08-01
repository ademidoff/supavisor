//go:build linux

package server

import (
	"fmt"
	"os"
	"strings"
)

// bootIDPath identifies the current boot. A process start time on Linux is
// measured in ticks since boot, so it means nothing on its own once the machine
// has restarted.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// startTimeField is the index of the process start time within /proc/<pid>/stat
// once the fields before and including the command name have been dropped. The
// start time is field 22 and the slice begins at field 3, so it sits at 19.
const startTimeField = 19

// processStartToken returns an opaque identifier for the current run of pid.
//
// A PID on its own does not identify a process: the kernel reuses PIDs, so a
// PID recorded before a crash may belong to something entirely unrelated by the
// time supavisor starts again. Field 22 of /proc/<pid>/stat is the start time in
// clock ticks since boot, which is fixed for the life of the process and
// distinguishes it from a later one that inherited its number.
func processStartToken(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", fmt.Errorf("failed to read process info for pid %d: %w", pid, err)
	}

	// The second field is the command name in parentheses and may itself
	// contain spaces and parentheses, so fields are counted from the last ')'.
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return "", fmt.Errorf("unrecognized /proc/%d/stat format", pid)
	}

	fields := strings.Fields(stat[end+1:])
	if len(fields) <= startTimeField {
		return "", fmt.Errorf("unrecognized /proc/%d/stat format", pid)
	}
	return fields[startTimeField], nil
}

// bootID identifies the running boot, so that process start times recorded
// before a restart are recognizable as belonging to a different one.
func bootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("failed to read boot id: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("%s is empty", bootIDPath)
	}
	return id, nil
}
