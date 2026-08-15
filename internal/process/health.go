package process

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

const (
	// maxProbeOutputBytes bounds how much of a failing exec probe's output is
	// carried into the log line.
	maxProbeOutputBytes = 200

	// probeWaitDelay bounds how long a failing probe's output is waited for
	// after it exits, so that a probe leaking a background child, which holds
	// the pipe open behind it, cannot hold the checker.
	probeWaitDelay = time.Second
)

// Health is what a program's health check says about it. It is separate from
// the process state, because a program can be RUNNING and not yet able to
// serve: a database in its bootstrap phase is alive but refuses connections.
type Health string

const (
	// HealthNone is reported when no health check is configured, and when the
	// program is not running, where readiness would mean nothing.
	HealthNone Health = "NONE"

	// HealthStarting is a configured check that has not passed yet
	HealthStarting Health = "STARTING"

	// HealthHealthy is a check that passed on its last attempt
	HealthHealthy Health = "HEALTHY"

	// HealthUnhealthy is a check that has failed retries times in a row
	HealthUnhealthy Health = "UNHEALTHY"
)

// probeFunc runs one health check attempt. A nil error means the program is
// ready to serve.
type probeFunc func(ctx context.Context) error

// SetHealthChangeCallback sets a callback for health changes
func (p *Process) SetHealthChangeCallback(fn func(name string, prevHealth, health Health)) {
	p.onHealthChange = fn
}

// GetHealth returns what the health check last reported. It is HealthNone for a
// program without a health check, and for one that is not running.
func (p *Process) GetHealth() Health {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.health == "" {
		return HealthNone
	}
	return p.health
}

// setHealth records a health result and calls the callback
func (p *Process) setHealth(health Health) {
	p.mu.Lock()
	prevHealth := p.health
	if prevHealth == "" {
		prevHealth = HealthNone
	}
	p.health = health
	p.mu.Unlock()

	if prevHealth == health {
		return
	}

	// The server logs the transition too, so this is the detail behind it
	p.logger.Debug("Health changed", "prev_health", prevHealth, "health", health)
	if p.onHealthChange != nil {
		p.onHealthChange(p.config.Name, prevHealth, health)
	}
}

// startHealthChecks begins probing this run. It is a no-op for a program
// without a health check, which stays at HealthNone.
func (p *Process) startHealthChecks(ctx context.Context) {
	check := p.config.HealthCheck
	if check == nil {
		return
	}

	probe, err := newProbe(p.config, p.logger)
	if err != nil {
		// Configuration validation rejects this, so reaching it means the
		// program was built in code rather than parsed from a file.
		p.logger.Error("Health check is unusable, not probing", "error", err)
		return
	}

	healthCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	p.mu.Lock()
	p.healthCancel = cancel
	p.healthDone = done
	p.mu.Unlock()

	// Set before returning rather than from the goroutine, so that a dependent
	// looking immediately after the start cannot see the previous run's result.
	p.setHealth(HealthStarting)

	go func() {
		defer close(done)
		p.probeLoop(healthCtx, check, probe)
	}()
}

// stopHealthChecks ends probing and clears the health, which means nothing for
// a program that is not running. It is safe to call more than once.
func (p *Process) stopHealthChecks() {
	p.mu.Lock()
	cancel, done := p.healthCancel, p.healthDone
	p.healthCancel, p.healthDone = nil, nil
	p.mu.Unlock()

	if cancel == nil {
		return
	}

	cancel()
	<-done
	p.setHealth(HealthNone)
}

// probeLoop checks the program until the run ends.
//
// A failing probe never stops or restarts the program: supavisor reports what
// it observes, and the restart policy stays tied to the process actually
// exiting. What health does decide is whether dependents waiting on it may
// start.
func (p *Process) probeLoop(ctx context.Context, check *config.HealthCheck, probe probeFunc) {
	ticker := time.NewTicker(check.Interval)
	defer ticker.Stop()

	// Probing straight away rather than after one interval keeps a program that
	// is ready immediately from paying the interval as startup latency.
	graceEnd := time.Now().Add(check.StartPeriod)
	everHealthy := false
	failures := 0

	for {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, check.Timeout)
		err := probe(attemptCtx)
		cancelAttempt()

		switch {
		case ctx.Err() != nil:
			return

		case err == nil:
			failures = 0
			everHealthy = true
			p.setHealth(HealthHealthy)

		default:
			failures++
			p.logger.Debug("Health check failed", "error", err, "consecutive_failures", failures)

			// Inside start_period a program that has never been healthy is
			// still starting up, which is the whole point of that window.
			if failures >= check.Retries && (everHealthy || !time.Now().Before(graceEnd)) {
				p.setHealth(HealthUnhealthy)
			}
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// newProbe builds the probe for a program's health check
func newProbe(cfg *config.ProgramConfig, logger *slog.Logger) (probeFunc, error) {
	check := cfg.HealthCheck

	switch {
	case check.Exec != "":
		return execProbe(cfg, logger), nil
	case check.TCP != "":
		return tcpProbe(check.TCP), nil
	case check.HTTP != "":
		return httpProbe(check.HTTP), nil
	}

	return nil, fmt.Errorf("health check has no exec command, tcp address or http url")
}

// execProbe runs a command and treats a zero exit status as ready. It runs
// where the program runs and with the program's environment, so that a check
// like pg_isready sees the same settings the program was given.
func execProbe(cfg *config.ProgramConfig, logger *slog.Logger) probeFunc {
	return func(ctx context.Context) error {
		parts := parseCommand(cfg.HealthCheck.Exec)
		if len(parts) == 0 {
			return fmt.Errorf("invalid health check command: %s", cfg.HealthCheck.Exec)
		}

		// Neither CommandContext nor CombinedOutput, because both of them wait
		// for the child and the reaper is the only thing allowed to do that. A
		// second waiter loses statuses, and a lost status here reads as a
		// failed probe: a healthy program intermittently marked UNHEALTHY, and
		// its dependents held back with it.
		//nolint:gosec,noctx // see above; the context is honored below instead
		cmd := exec.Command(parts[0], parts[1:]...)
		cmd.Dir = cfg.Directory
		cmd.Env = processEnv(cfg)
		// Its own group, so a timed-out probe can be killed along with anything
		// it spawned rather than just the command itself.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("failed to create probe pipe: %w", err)
		}
		// Closing the read end unblocks the drain goroutine below, whatever
		// happens to the probe itself.
		defer func() { _ = readEnd.Close() }()

		// An *os.File is handed to the child as-is. Any other writer makes
		// os/exec start a copier that only Wait() can finish.
		cmd.Stdout = writeEnd
		cmd.Stderr = writeEnd

		var pid int
		exited, err := reaperFor(logger).spawn(func() (int, error) {
			if startErr := cmd.Start(); startErr != nil {
				return 0, startErr
			}
			pid = cmd.Process.Pid
			return pid, nil
		})
		// The child holds its own copy now. Ours would keep the read below from
		// ever reaching EOF.
		_ = writeEnd.Close()
		if err != nil {
			return err
		}

		output := make(chan []byte, 1)
		go func() {
			// A read error means the probe is gone or the pipe was closed under
			// us; either way whatever arrived is all the output there is.
			data, _ := io.ReadAll(readEnd) //nolint:errcheck
			output <- data
		}()

		select {
		case status := <-exited:
			if exitCodeOfStatus(status) == 0 {
				return nil
			}
			return probeFailure(status, output)
		case <-ctx.Done():
			if pid > 0 {
				//nolint:errcheck // the probe may already have exited on its own
				_ = SignalGroup(pid, syscall.SIGKILL)
			}
			return fmt.Errorf("health check timed out: %w", ctx.Err())
		}
	}
}

// probeFailure describes a probe that exited non-zero, quoting its output when
// it produced any. The wait is bounded because a probe that leaked a background
// child leaves the pipe open behind it, and the checker must not be held.
func probeFailure(status syscall.WaitStatus, output <-chan []byte) error {
	err := exitErrorOf(status)

	select {
	case data := <-output:
		if line := firstLine(data); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
	case <-time.After(probeWaitDelay):
	}
	return err
}

// tcpProbe connects to an address and treats a completed connection as ready
func tcpProbe(address string) probeFunc {
	return func(ctx context.Context) error {
		var dialer net.Dialer

		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

// httpProbe issues a GET and treats any non-error status as ready
func httpProbe(target string) probeFunc {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("health check returned status %d", resp.StatusCode)
		}
		return nil
	}
}

// firstLine reduces probe output to something that fits in a log line
func firstLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}

	if idx := strings.IndexByte(text, '\n'); idx != -1 {
		text = text[:idx]
	}
	if len(text) > maxProbeOutputBytes {
		text = text[:maxProbeOutputBytes] + "..."
	}
	return text
}
