package process

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/ademidoff/supavisor/internal/config"
)

const (
	// maxProbeOutputBytes bounds how much of a failing exec probe's output is
	// carried into the log line.
	maxProbeOutputBytes = 200

	// probeWaitDelay is how long an exec probe's descriptors stay open after it
	// exits, so that a probe leaking a background child cannot hold the checker.
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

	probe, err := newProbe(p.config)
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
func newProbe(cfg *config.ProgramConfig) (probeFunc, error) {
	check := cfg.HealthCheck

	switch {
	case check.Exec != "":
		return execProbe(cfg), nil
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
func execProbe(cfg *config.ProgramConfig) probeFunc {
	return func(ctx context.Context) error {
		parts := parseCommand(cfg.HealthCheck.Exec)
		if len(parts) == 0 {
			return fmt.Errorf("invalid health check command: %s", cfg.HealthCheck.Exec)
		}

		cmd := exec.CommandContext(ctx, parts[0], parts[1:]...) //nolint:gosec
		cmd.Dir = cfg.Directory
		cmd.Env = processEnv(cfg)
		cmd.WaitDelay = probeWaitDelay

		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if output := firstLine(out); output != "" {
			return fmt.Errorf("%w: %s", err, output)
		}
		return err
	}
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
