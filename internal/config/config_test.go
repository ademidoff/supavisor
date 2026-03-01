package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.yml")

	configContent := `supavisor:
  logfile: /var/log/supavisor/supavisor.log
  pidfile: /var/run/supavisor.pid
  socket: /tmp/supavisor.sock
  log_format: json

programs:
  testapp:
    command: /usr/bin/testapp
    directory: /opt/testapp
    autostart: true
    autorestart: always
    startsecs: 5
    max_restarts: 3
    depends_on:
      - database
    stdout_logfile: /var/log/testapp/stdout.log
    stdout_logfile_maxbytes: 10MB
    stdout_logfile_backups: 5
    stdout_logfile_maxage: 7
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	cfg, err := ParseConfigFile(configPath)
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}

	if cfg.Supavisor.LogFile != "/var/log/supavisor/supavisor.log" {
		t.Errorf("Expected logfile /var/log/supavisor/supavisor.log, got %s", cfg.Supavisor.LogFile)
	}

	if cfg.Supavisor.LogFormat != "json" {
		t.Errorf("Expected log_format json, got %s", cfg.Supavisor.LogFormat)
	}

	prog, exists := cfg.Programs["testapp"]
	if !exists {
		t.Fatal("Program 'testapp' not found in config")
	}

	if prog.Command != "/usr/bin/testapp" {
		t.Errorf("Expected command /usr/bin/testapp, got %s", prog.Command)
	}

	if prog.Directory != "/opt/testapp" {
		t.Errorf("Expected directory /opt/testapp, got %s", prog.Directory)
	}

	if !prog.Autostart {
		t.Error("Expected autostart to be true")
	}

	if prog.Autorestart != RestartAlways {
		t.Errorf("Expected autorestart to be always, got %s", prog.Autorestart)
	}

	if len(prog.DependsOn) != 1 || prog.DependsOn[0] != "database" {
		t.Errorf("Expected depends_on to be [database], got %v", prog.DependsOn)
	}
}

func TestParseConfigFile_DefaultLogFormat(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test_default.yml")

	configContent := `supavisor:
  logfile: /var/log/supavisor/supavisor.log

programs:
  testapp:
    command: /usr/bin/testapp
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	cfg, err := ParseConfigFile(configPath)
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}

	if cfg.Supavisor.LogFormat != "text" {
		t.Errorf("Expected default log_format text, got %s", cfg.Supavisor.LogFormat)
	}
}

func TestParseConfigFile_LogLevel(t *testing.T) {
	tests := []struct {
		name          string
		configContent string
		expectedLevel string
	}{
		{
			name: "explicit debug level",
			configContent: `supavisor:
  log_level: debug

programs:
  testapp:
    command: /usr/bin/testapp
`,
			expectedLevel: "debug",
		},
		{
			name: "explicit info level",
			configContent: `supavisor:
  log_level: info

programs:
  testapp:
    command: /usr/bin/testapp
`,
			expectedLevel: "info",
		},
		{
			name: "explicit warn level",
			configContent: `supavisor:
  log_level: warn

programs:
  testapp:
    command: /usr/bin/testapp
`,
			expectedLevel: "warn",
		},
		{
			name: "explicit error level",
			configContent: `supavisor:
  log_level: error

programs:
  testapp:
    command: /usr/bin/testapp
`,
			expectedLevel: "error",
		},
		{
			name: "default log level when not specified",
			configContent: `supavisor:
  logfile: /var/log/supavisor/supavisor.log

programs:
  testapp:
    command: /usr/bin/testapp
`,
			expectedLevel: "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "test.yml")

			err := os.WriteFile(configPath, []byte(tt.configContent), 0644)
			if err != nil {
				t.Fatalf("Failed to create test config file: %v", err)
			}

			cfg, err := ParseConfigFile(configPath)
			if err != nil {
				t.Fatalf("Failed to parse config file: %v", err)
			}

			if cfg.Supavisor.LogLevel != tt.expectedLevel {
				t.Errorf("Expected log_level %s, got %s", tt.expectedLevel, cfg.Supavisor.LogLevel)
			}
		})
	}
}

func TestParseConfigFile_EnvironmentMap(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.yml")

	configContent := `supavisor: {}

programs:
  testapp:
    command: /usr/bin/testapp
    environment:
      KEY1: value1
      KEY2: "value with spaces"
      KEY3: "value with, comma"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	cfg, err := ParseConfigFile(configPath)
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}

	prog := cfg.Programs["testapp"]
	if prog.Environment["KEY1"] != "value1" {
		t.Errorf("Expected KEY1=value1, got %s", prog.Environment["KEY1"])
	}
	if prog.Environment["KEY2"] != "value with spaces" {
		t.Errorf("Expected KEY2 with spaces, got %s", prog.Environment["KEY2"])
	}
	if prog.Environment["KEY3"] != "value with, comma" {
		t.Errorf("Expected KEY3 with comma, got %s", prog.Environment["KEY3"])
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"10MB", 10 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"500KB", 500 * 1024},
		{"100", 100},
		{"", 50 * 1024 * 1024}, // Default 50MB
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseBytes(tt.input)
			if result != tt.expected {
				t.Errorf("parseBytes(%s) = %d, expected %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseEnvironmentVariables(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
		wantErr  bool
	}{
		{
			name:  "simple variables",
			input: "KEY1=value1,KEY2=value2",
			expected: map[string]string{
				"KEY1": "value1",
				"KEY2": "value2",
			},
			wantErr: false,
		},
		{
			name:  "quoted value with comma",
			input: `KEY1=value1,KEY2="value with, comma",KEY3=value3`,
			expected: map[string]string{
				"KEY1": "value1",
				"KEY2": "value with, comma",
				"KEY3": "value3",
			},
			wantErr: false,
		},
		{
			name:  "single quoted value",
			input: `KEY1='value with, comma'`,
			expected: map[string]string{
				"KEY1": "value with, comma",
			},
			wantErr: false,
		},
		{
			name:    "missing equals",
			input:   "KEY1value1",
			wantErr: true,
		},
		{
			name:    "empty key",
			input:   "=value1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseEnvironmentVariables(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseEnvironmentVariables() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(result) != len(tt.expected) {
					t.Errorf("Expected %d variables, got %d", len(tt.expected), len(result))
				}
				for k, v := range tt.expected {
					if result[k] != v {
						t.Errorf("Expected %s=%s, got %s=%s", k, v, k, result[k])
					}
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				Programs: map[string]*ProgramConfig{
					"app1": {
						Name:      "app1",
						DependsOn: []string{},
					},
					"app2": {
						Name:      "app2",
						DependsOn: []string{"app1"},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "circular dependency",
			config: &Config{
				Programs: map[string]*ProgramConfig{
					"app1": {
						Name:      "app1",
						DependsOn: []string{"app2"},
					},
					"app2": {
						Name:      "app2",
						DependsOn: []string{"app1"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "missing dependency",
			config: &Config{
				Programs: map[string]*ProgramConfig{
					"app1": {
						Name:      "app1",
						DependsOn: []string{"nonexistent"},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
