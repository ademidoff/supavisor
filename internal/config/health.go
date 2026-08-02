package config

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// defaultProbeInterval is how often a health check runs. It is short
	// because a dependent waiting on the check pays it as startup latency.
	defaultProbeInterval = 2 * time.Second

	// defaultProbeTimeout bounds a single attempt, so that a probe which hangs
	// holds up the next one rather than the checker for good.
	defaultProbeTimeout = 5 * time.Second

	// defaultProbeRetries is how many consecutive failures mark a program that
	// was healthy as unhealthy.
	defaultProbeRetries = 3

	// probeHTTP names the http probe kind, which is also the url scheme it
	// accepts alongside https.
	probeHTTP = "http"
)

// HealthCheck describes how to tell whether a program is ready to serve, as
// opposed to merely running. Exactly one of Exec, TCP and HTTP is set.
type HealthCheck struct {
	Exec        string
	TCP         string
	HTTP        string
	Interval    time.Duration
	Timeout     time.Duration
	StartPeriod time.Duration
	Retries     int
}

type healthCheckFile struct {
	Retries     *int   `yaml:"retries"`
	Exec        string `yaml:"exec"`
	TCP         string `yaml:"tcp"`
	HTTP        string `yaml:"http"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	StartPeriod string `yaml:"start_period"`
}

// convertHealthCheck resolves a raw health_check block, or returns nil for a
// program that does not have one
func convertHealthCheck(raw *healthCheckFile) (*HealthCheck, error) {
	if raw == nil {
		return nil, nil
	}

	check := &HealthCheck{
		Exec:    strings.TrimSpace(raw.Exec),
		TCP:     strings.TrimSpace(raw.TCP),
		HTTP:    strings.TrimSpace(raw.HTTP),
		Retries: intOrDefault(raw.Retries, defaultProbeRetries),
	}

	if err := validateProbeTarget(check); err != nil {
		return nil, err
	}

	var err error
	if check.Interval, err = parseDuration("health_check.interval", raw.Interval, defaultProbeInterval); err != nil {
		return nil, err
	}
	if check.Timeout, err = parseDuration("health_check.timeout", raw.Timeout, defaultProbeTimeout); err != nil {
		return nil, err
	}
	if check.StartPeriod, err = parseDuration("health_check.start_period", raw.StartPeriod, 0); err != nil {
		return nil, err
	}

	switch {
	case check.Interval <= 0:
		return nil, fmt.Errorf("invalid health_check.interval: %s (must be positive)", check.Interval)
	case check.Timeout <= 0:
		return nil, fmt.Errorf("invalid health_check.timeout: %s (must be positive)", check.Timeout)
	case check.StartPeriod < 0:
		return nil, fmt.Errorf("invalid health_check.start_period: %s (must not be negative)", check.StartPeriod)
	case check.Retries < 1:
		return nil, fmt.Errorf("invalid health_check.retries: %d (must be at least 1)", check.Retries)
	}

	return check, nil
}

// validateProbeTarget checks that exactly one kind of probe is configured and
// that it is usable, so that a typo fails at startup rather than on every
// attempt for the lifetime of the program.
func validateProbeTarget(check *HealthCheck) error {
	kinds := map[string]string{"exec": check.Exec, "tcp": check.TCP, probeHTTP: check.HTTP}
	configured := make([]string, 0, len(kinds))
	for kind, value := range kinds {
		if value != "" {
			configured = append(configured, kind)
		}
	}
	sort.Strings(configured)

	switch {
	case len(configured) == 0:
		return fmt.Errorf("health_check needs one of exec, tcp or http")
	case len(configured) > 1:
		return fmt.Errorf("health_check sets %s: only one of exec, tcp or http may be used", strings.Join(configured, " and "))
	}

	if check.TCP != "" {
		if _, _, err := net.SplitHostPort(check.TCP); err != nil {
			return fmt.Errorf("invalid health_check.tcp: %s (expected host:port)", check.TCP)
		}
	}
	if check.HTTP != "" {
		parsed, err := url.Parse(check.HTTP)
		if err != nil {
			return fmt.Errorf("invalid health_check.http: %s (%w)", check.HTTP, err)
		}
		if (parsed.Scheme != probeHTTP && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("invalid health_check.http: %s (expected an http:// or https:// url)", check.HTTP)
		}
	}

	return nil
}

// parseDuration parses a duration such as 2s or 500ms. An empty value takes the
// default; anything unparseable is an error rather than a silent fallback, for
// the same reason a mistyped log size is.
func parseDuration(field, value string, def time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return def, nil
	}

	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %s (expected a duration such as 2s or 500ms)", field, value)
	}
	return parsed, nil
}
