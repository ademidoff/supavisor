package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseConfigString parses an inline configuration document
func parseConfigString(t *testing.T, content string) (*Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "supavisor.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	return ParseConfigFile(path)
}

// TestDependsOn_MappingFormCarriesConditions covers the form that can wait for
// readiness rather than only for the process being up.
func TestDependsOn_MappingFormCarriesConditions(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  db:
    command: /usr/bin/postgres
    health_check:
      exec: /bin/true
  api:
    command: /usr/bin/api
    depends_on:
      db:
        condition: healthy
  logs:
    command: /usr/bin/tailer
    depends_on:
      db:
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	api := cfg.Programs["api"].DependsOn
	if len(api) != 1 || api[0].Name != "db" {
		t.Fatalf("Expected api to depend on db, got %v", api)
	}
	if api[0].Condition != ConditionHealthy {
		t.Errorf("Expected api to wait for db to be healthy, got %s", api[0].Condition)
	}

	// A bare name under the mapping form means the default condition, the same
	// as writing it in the list form.
	logs := cfg.Programs["logs"].DependsOn
	if len(logs) != 1 || logs[0].Condition != ConditionStarted {
		t.Errorf("Expected a bare dependency to wait for started, got %v", logs)
	}
}

// TestDependsOn_ListFormIsUnchanged is the backward compatibility check: the
// form every existing configuration uses still means "wait for RUNNING".
func TestDependsOn_ListFormIsUnchanged(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  db:
    command: /usr/bin/postgres
    health_check:
      exec: /bin/true
  api:
    command: /usr/bin/api
    depends_on:
      - db
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	// Declaring a health check must not silently start gating dependents that
	// never asked to wait for it.
	deps := cfg.Programs["api"].DependsOn
	if len(deps) != 1 || deps[0].Condition != ConditionStarted {
		t.Errorf("Expected the list form to keep waiting for started only, got %v", deps)
	}
}

func TestDependsOn_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "unknown condition",
			content: `programs:
  db:
    command: /usr/bin/postgres
  api:
    command: /usr/bin/api
    depends_on:
      db:
        condition: ready
`,
			wantErr: "must be started or healthy",
		},
		{
			name: "unknown key in an entry",
			content: `programs:
  db:
    command: /usr/bin/postgres
  api:
    command: /usr/bin/api
    depends_on:
      db:
        when: healthy
`,
			wantErr: "unknown key when",
		},
		{
			name: "entry is not a mapping",
			content: `programs:
  db:
    command: /usr/bin/postgres
  api:
    command: /usr/bin/api
    depends_on:
      db: healthy
`,
			wantErr: "expected a mapping with a condition key",
		},
		{
			name: "depends_on is a scalar",
			content: `programs:
  db:
    command: /usr/bin/postgres
  api:
    command: /usr/bin/api
    depends_on: db
`,
			wantErr: "expected a list of program names",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfigString(t, tt.content)
			if err == nil {
				t.Fatal("Expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Expected the error to mention %s, got: %v", tt.wantErr, err)
			}
		})
	}
}

// TestValidate_HealthyConditionNeedsAHealthCheck rejects a wait that could
// never be satisfied, rather than leaving the dependent stopped for good.
func TestValidate_HealthyConditionNeedsAHealthCheck(t *testing.T) {
	cfg, err := parseConfigString(t, `programs:
  db:
    command: /usr/bin/postgres
  api:
    command: /usr/bin/api
    depends_on:
      db:
        condition: healthy
`)
	if err != nil {
		t.Fatalf("ParseConfigFile failed: %v", err)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Expected waiting on the health of a program without a health check to be rejected")
	}
	if !strings.Contains(err.Error(), "no health_check") {
		t.Errorf("Expected the error to name the missing health_check, got: %v", err)
	}
}
