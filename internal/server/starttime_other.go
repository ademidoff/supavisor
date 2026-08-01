//go:build !linux && !darwin

package server

import "fmt"

// processStartToken is unavailable here, so orphans left by a crashed daemon
// are reported rather than killed: without a way to tell a recorded PID from a
// later process that reused it, killing would risk stopping something unrelated.
func processStartToken(pid int) (string, error) {
	return "", fmt.Errorf("process identity is not available on %s", "this platform")
}
