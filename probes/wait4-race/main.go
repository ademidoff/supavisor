//go:build ignore

// Why supavisor does not simply add a wait4(-1) goroutine.
//
// Reaping orphans means waiting for children we did not start, and the obvious
// way to do that is a goroutine calling wait4(-1) alongside everything else.
// This program shows why that is not safe in a process that also uses os/exec.
//
// os/exec waits on a specific PID. A concurrent wait4(-1) takes whatever has
// exited, that PID included, and cmd.Wait() then finds nothing left to wait
// for and returns an error instead of the exit code.
//
// For supavisor that lost exit code is not cosmetic: autorestart: unexpected
// reads it, so a clean exit becomes an apparent crash, the program is restarted
// when it should have been left alone, and max_restarts eventually parks it in
// FATAL. It happens a few times in a hundred, which is rare enough to pass a
// test run and show up later under load.
//
// The conclusion is in internal/process/reaper.go: exactly one waiter in the
// daemon, dispatching each status to whoever owns that PID. Nothing else calls
// wait4 or cmd.Wait().
//
// Run it with:
//
//	go run probes/wait4-race/main.go
package main

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// runs is enough to make a few-percent loss rate show up reliably.
const runs = 200

// wantExit is what every child exits with, so any other answer is a lost status
// rather than a coincidence.
const wantExit = 7

func main() {
	stop := make(chan struct{})

	// The naive reaper: the thing this program exists to warn against.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}

			var status syscall.WaitStatus
			pid, _ := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid > 0 {
				fmt.Printf("  reaper swallowed pid=%d exit=%d\n", pid, status.ExitStatus())
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var survived, lost int
	for i := range runs {
		cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", wantExit))
		if err := cmd.Start(); err != nil {
			fmt.Printf("  run %d: start failed: %v\n", i, err)
			continue
		}

		// A non-zero exit always yields an ExitError, so the error being
		// non-nil is expected. What matters is whether the code survived.
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == wantExit {
			survived++
			continue
		}

		lost++
		fmt.Printf("  run %d: LOST — err=%v\n", i, err)
	}
	close(stop)

	fmt.Printf("\nexit status survived: %d/%d, LOST: %d/%d\n", survived, runs, lost, runs)
	if lost > 0 {
		fmt.Println("\nThis is the failure mode. One waiter per process, or exit codes go missing.")
	}
}
