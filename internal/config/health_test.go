package config

import (
	"strings"
	"testing"
	"time"
)

func TestHealthCheck_Defaults(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  db:
    command: /usr/bin/postgres
    health_check:
      exec: pg_isready -q
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	check := cfg.Programs["db"].HealthCheck
	if check == nil {
		t.Fatal("Expected a health check")
	}
	if check.Exec != "pg_isready -q" {
		t.Errorf("Expected the exec command to be kept, got %s", check.Exec)
	}
	if check.Interval != defaultProbeInterval {
		t.Errorf("Expected the default interval %s, got %s", defaultProbeInterval, check.Interval)
	}
	if check.Timeout != defaultProbeTimeout {
		t.Errorf("Expected the default timeout %s, got %s", defaultProbeTimeout, check.Timeout)
	}
	if check.Retries != defaultProbeRetries {
		t.Errorf("Expected the default retries %d, got %d", defaultProbeRetries, check.Retries)
	}
	if check.StartPeriod != 0 {
		t.Errorf("Expected no start period by default, got %s", check.StartPeriod)
	}
}

// TestHealthCheck_NoneConfigured keeps a program without a health check free of
// one, since that is what every existing configuration looks like.
func TestHealthCheck_NoneConfigured(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  db:
    command: /usr/bin/postgres
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	if cfg.Programs["db"].HealthCheck != nil {
		t.Error("A program without a health_check block should not get one")
	}
}

func TestHealthCheck_ExplicitSettings(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  api:
    command: /usr/bin/api
    health_check:
      http: http://127.0.0.1:8080/readyz
      interval: 500ms
      timeout: 1s
      start_period: 1m
      retries: 5
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	check := cfg.Programs["api"].HealthCheck
	if check.HTTP != "http://127.0.0.1:8080/readyz" {
		t.Errorf("Expected the http url to be kept, got %s", check.HTTP)
	}
	if check.Interval != 500*time.Millisecond {
		t.Errorf("Expected an interval of 500ms, got %s", check.Interval)
	}
	if check.Timeout != time.Second {
		t.Errorf("Expected a timeout of 1s, got %s", check.Timeout)
	}
	if check.StartPeriod != time.Minute {
		t.Errorf("Expected a start period of 1m, got %s", check.StartPeriod)
	}
	if check.Retries != 5 {
		t.Errorf("Expected 5 retries, got %d", check.Retries)
	}
}

// TestHealthCheck_RejectsBadInput keeps a mistyped probe from being discovered
// one failed attempt at a time, for the lifetime of the program.
func TestHealthCheck_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		check   string
		wantErr string
	}{
		{
			name:    "no probe kind",
			check:   "      interval: 2s",
			wantErr: "needs one of exec, tcp or http",
		},
		{
			name:    "two probe kinds",
			check:   "      exec: /bin/true\n      tcp: 127.0.0.1:5432",
			wantErr: "only one of exec, tcp or http",
		},
		{
			name:    "unparseable interval",
			check:   "      exec: /bin/true\n      interval: soon",
			wantErr: "invalid health_check.interval",
		},
		{
			name:    "zero interval",
			check:   "      exec: /bin/true\n      interval: 0s",
			wantErr: "must be positive",
		},
		{
			name:    "no retries",
			check:   "      exec: /bin/true\n      retries: 0",
			wantErr: "must be at least 1",
		},
		{
			name:    "tcp address without a port",
			check:   "      tcp: 127.0.0.1",
			wantErr: "expected host:port",
		},
		{
			name:    "unparseable http url",
			check:   "      http: 127.0.0.1:8080/readyz",
			wantErr: "invalid health_check.http",
		},
		{
			name:    "http url with another scheme",
			check:   "      http: ftp://127.0.0.1/readyz",
			wantErr: "expected an http:// or https:// url",
		},
		{
			name:    "unknown setting",
			check:   "      exec: /bin/true\n      retires: 2",
			wantErr: "retires",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfigString(t, "programs:\n  db:\n    command: /usr/bin/postgres\n    health_check:\n"+tt.check+"\n")
			if err == nil {
				t.Fatal("Expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Expected the error to mention %s, got: %v", tt.wantErr, err)
			}
		})
	}
}
