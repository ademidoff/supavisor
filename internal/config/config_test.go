package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ademidoff/supavisor/internal/dependency"
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

	err := os.WriteFile(configPath, []byte(configContent), 0o644)
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

	err := os.WriteFile(configPath, []byte(configContent), 0o644)
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

			err := os.WriteFile(configPath, []byte(tt.configContent), 0o644)
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

	err := os.WriteFile(configPath, []byte(configContent), 0o644)
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
			result, err := parseBytes("stdout_logfile_maxbytes", tt.input)
			if err != nil {
				t.Fatalf("parseBytes(%s) failed: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Errorf("parseBytes(%s) = %d, expected %d", tt.input, result, tt.expected)
			}
		})
	}
}

// TestParseBytes_RejectsUnparseableSizes guards against a typo in a size limit
// silently becoming the 50MB default.
func TestParseBytes_RejectsUnparseableSizes(t *testing.T) {
	for _, input := range []string{"10 MB please", "abc", "10TB", "-5MB", "1.5MB"} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseBytes("stdout_logfile_maxbytes", input); err == nil {
				t.Errorf("Expected %s to be rejected", input)
			}
		})
	}
}

func TestParseConfig_NoFragmentDir(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")

	main := `supavisor:
  pidfile: /tmp/sv.pid
programs:
  app1:
    command: /bin/true
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}

	cfg, err := ParseConfig(mainPath)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if _, ok := cfg.Programs["app1"]; !ok {
		t.Fatal("expected program app1 to be loaded")
	}
	if len(cfg.Programs) != 1 {
		t.Errorf("expected 1 program, got %d", len(cfg.Programs))
	}
}

func TestParseConfig_MergesFragments(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `supavisor:
  pidfile: /tmp/sv.pid
programs:
  base:
    command: /bin/true
`
	frag1 := `programs:
  extra1:
    command: /bin/sleep 1
    depends_on:
      - base
`
	frag2 := `programs:
  extra2:
    command: /bin/sleep 2
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "10-a.yml"), []byte(frag1), 0o644); err != nil {
		t.Fatalf("write frag1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "20-b.yaml"), []byte(frag2), 0o644); err != nil {
		t.Fatalf("write frag2: %v", err)
	}
	// non-yaml files must be ignored
	if err := os.WriteFile(filepath.Join(fragDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	cfg, err := ParseConfig(mainPath)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	for _, name := range []string{"base", "extra1", "extra2"} {
		if _, ok := cfg.Programs[name]; !ok {
			t.Errorf("expected program %s in merged config", name)
		}
	}
	if cfg.Supavisor.PidFile != "/tmp/sv.pid" {
		t.Errorf("expected pidfile from main file, got %s", cfg.Supavisor.PidFile)
	}
	if got := cfg.Programs["extra1"].DependsOn; len(got) != 1 || got[0] != "base" {
		t.Errorf("expected extra1 depends_on=[base], got %v", got)
	}
}

func TestParseConfig_DuplicateProgramAcrossFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `programs:
  shared:
    command: /bin/true
`
	frag := `programs:
  shared:
    command: /bin/false
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	fragPath := filepath.Join(fragDir, "10-dup.yml")
	if err := os.WriteFile(fragPath, []byte(frag), 0o644); err != nil {
		t.Fatalf("write frag: %v", err)
	}

	_, err := ParseConfig(mainPath)
	if err == nil {
		t.Fatal("expected duplicate program error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate program shared") {
		t.Errorf("error should mention duplicate program: %s", msg)
	}
	if !strings.Contains(msg, mainPath) || !strings.Contains(msg, fragPath) {
		t.Errorf("error should reference both source files; got: %s", msg)
	}
}

func TestParseConfig_DuplicateProgramAcrossFragments(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `programs:
  base:
    command: /bin/true
`
	frag1 := `programs:
  twin:
    command: /bin/sleep 1
`
	frag2 := `programs:
  twin:
    command: /bin/sleep 2
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	frag1Path := filepath.Join(fragDir, "10-a.yml")
	frag2Path := filepath.Join(fragDir, "20-b.yml")
	if err := os.WriteFile(frag1Path, []byte(frag1), 0o644); err != nil {
		t.Fatalf("write frag1: %v", err)
	}
	if err := os.WriteFile(frag2Path, []byte(frag2), 0o644); err != nil {
		t.Fatalf("write frag2: %v", err)
	}

	_, err := ParseConfig(mainPath)
	if err == nil {
		t.Fatal("expected duplicate program error across fragments")
	}
	msg := err.Error()
	if !strings.Contains(msg, frag1Path) || !strings.Contains(msg, frag2Path) {
		t.Errorf("error should reference both fragment files; got: %s", msg)
	}
}

func TestParseConfig_FragmentWithSupavisorSectionIsError(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `programs:
  base:
    command: /bin/true
`
	frag := `supavisor:
  pidfile: /tmp/other.pid
programs:
  extra:
    command: /bin/true
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "10.yml"), []byte(frag), 0o644); err != nil {
		t.Fatalf("write frag: %v", err)
	}

	_, err := ParseConfig(mainPath)
	if err == nil {
		t.Fatal("expected error when fragment defines a supavisor section")
	}
	if !strings.Contains(err.Error(), "must not define a supavisor section") {
		t.Errorf("unexpected error message: %s", err.Error())
	}
}

// TestParseConfig_MultifileFixture loads the checked-in testdata fixture and
// verifies that dependencies declared across multiple files resolve into a
// single valid topological order.
func TestParseConfig_MultifileFixture(t *testing.T) {
	mainPath := filepath.Join("testdata", "multifile", "supavisor.yml")

	cfg, err := ParseConfig(mainPath)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	expected := []string{"database", "cache", "webapp", "worker"}
	for _, name := range expected {
		if _, ok := cfg.Programs[name]; !ok {
			t.Errorf("expected program %s in merged config", name)
		}
	}
	if len(cfg.Programs) != len(expected) {
		t.Errorf("expected %d programs, got %d", len(expected), len(cfg.Programs))
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed on merged multi-file config: %v", err)
	}

	g := dependency.NewGraph()
	for name, prog := range cfg.Programs {
		g.AddNode(name, prog.DependsOn)
	}
	order, err := g.TopologicalSort()
	if err != nil {
		t.Fatalf("TopologicalSort failed: %v", err)
	}

	pos := make(map[string]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	mustPrecede := [][2]string{
		{"database", "cache"},
		{"database", "webapp"},
		{"cache", "webapp"},
		{"cache", "worker"},
	}
	for _, pair := range mustPrecede {
		before, after := pair[0], pair[1]
		if pos[before] >= pos[after] {
			t.Errorf("expected %s to come before %s in order %v", before, after, order)
		}
	}
}

// TestParseConfig_MissingDependencyAcrossFiles verifies that a missing
// dependency is still caught by Validate when the dependee is in no file.
func TestParseConfig_MissingDependencyAcrossFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `programs:
  app:
    command: /bin/true
    depends_on:
      - ghost
`
	frag := `programs:
  sidecar:
    command: /bin/true
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "10.yml"), []byte(frag), 0o644); err != nil {
		t.Fatalf("write frag: %v", err)
	}

	cfg, err := ParseConfig(mainPath)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate to fail on missing cross-file dependency")
	}
}

// TestParseConfig_CircularDependencyAcrossFiles verifies a cycle that spans
// files is caught.
func TestParseConfig_CircularDependencyAcrossFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mainPath := filepath.Join(tmpDir, "supavisor.yml")
	fragDir := filepath.Join(tmpDir, "supavisor.d")
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatalf("mkdir fragments: %v", err)
	}

	main := `programs:
  a:
    command: /bin/true
    depends_on:
      - b
`
	frag := `programs:
  b:
    command: /bin/true
    depends_on:
      - a
`
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "10.yml"), []byte(frag), 0o644); err != nil {
		t.Fatalf("write frag: %v", err)
	}

	cfg, err := ParseConfig(mainPath)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate to detect circular dependency across files")
	}
}

// TestFragmentDir verifies the sibling drop-in directory resolution.
func TestFragmentDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/etc/supavisor/supavisor.yml", "/etc/supavisor/supavisor.d"},
		{"./supavisor.yaml", "supavisor.d"},
		{"/tmp/custom.yml", "/tmp/custom.d"},
	}
	for _, c := range cases {
		got := fragmentDir(c.in)
		if got != c.want {
			t.Errorf("fragmentDir(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestListFragmentFiles_OrderingAndFiltering verifies lexical ordering and that
// non-yaml entries and subdirectories are skipped.
func TestListFragmentFiles_OrderingAndFiltering(t *testing.T) {
	dir := t.TempDir()
	names := []string{"20-b.yml", "10-a.yaml", "30-c.yml", "skip.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("programs: {}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	got, err := listFragmentFiles(dir)
	if err != nil {
		t.Fatalf("listFragmentFiles: %v", err)
	}
	wantBases := []string{"10-a.yaml", "20-b.yml", "30-c.yml"}
	if len(got) != len(wantBases) {
		t.Fatalf("expected %d files, got %d (%v)", len(wantBases), len(got), got)
	}
	for i, p := range got {
		if filepath.Base(p) != wantBases[i] {
			t.Errorf("position %d: got %s, want %s", i, filepath.Base(p), wantBases[i])
		}
	}
	if slices.Contains(got, filepath.Join(dir, "skip.txt")) {
		t.Error("non-yaml file should not be included")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		config  *Config
		name    string
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

// TestParseConfigFile_RejectsUnknownKeys guards against a typo in a setting
// being ignored and the program silently running on defaults.
func TestParseConfigFile_RejectsUnknownKeys(t *testing.T) {
	tests := map[string]string{
		"misspelled program setting": `
programs:
  app:
    command: /bin/true
    autorestrt: always
`,
		"unknown program setting": `
programs:
  app:
    command: /bin/true
    thisDoesNotExist: 42
`,
		"unknown daemon setting": `
supavisor:
  pidfle: /tmp/x.pid
programs:
  app:
    command: /bin/true
`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "supavisor.yml")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}
			if _, err := ParseConfigFile(path); err == nil {
				t.Error("Expected an unknown key to be rejected")
			}
		})
	}
}

// TestParseConfigFile_ZeroIsNotTheSameAsUnset covers settings whose zero value
// is meaningful: max_restarts: 0 asks for no retries, and used to be replaced
// by the default of 3.
func TestParseConfigFile_ZeroIsNotTheSameAsUnset(t *testing.T) {
	content := `
programs:
  explicit:
    command: /bin/true
    startsecs: 0
    max_restarts: 0
    stopwaitsecs: 0
    priority: 0
  defaulted:
    command: /bin/true
`
	path := filepath.Join(t.TempDir(), "supavisor.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := ParseConfigFile(path)
	if err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}

	explicit := cfg.Programs["explicit"]
	if explicit.StartSecs != 0 {
		t.Errorf("startsecs: 0 became %d", explicit.StartSecs)
	}
	if explicit.MaxRestarts != 0 {
		t.Errorf("max_restarts: 0 became %d", explicit.MaxRestarts)
	}
	if explicit.StopWaitSecs != 0 {
		t.Errorf("stopwaitsecs: 0 became %d", explicit.StopWaitSecs)
	}
	if explicit.Priority != 0 {
		t.Errorf("priority: 0 became %d", explicit.Priority)
	}

	// An absent setting still takes its default.
	defaulted := cfg.Programs["defaulted"]
	if defaulted.StartSecs != defaultStartSecs {
		t.Errorf("Unset startsecs = %d, expected %d", defaulted.StartSecs, defaultStartSecs)
	}
	if defaulted.MaxRestarts != defaultMaxRestarts {
		t.Errorf("Unset max_restarts = %d, expected %d", defaulted.MaxRestarts, defaultMaxRestarts)
	}
	if defaulted.StopWaitSecs != defaultStopWaitSecs {
		t.Errorf("Unset stopwaitsecs = %d, expected %d", defaulted.StopWaitSecs, defaultStopWaitSecs)
	}
	if defaulted.Priority != defaultPriority {
		t.Errorf("Unset priority = %d, expected %d", defaulted.Priority, defaultPriority)
	}
}

func TestParseConfigFile_RejectsAnInvalidStopSignal(t *testing.T) {
	content := `
programs:
  app:
    command: /bin/true
    stopsignal: BANANA
`
	path := filepath.Join(t.TempDir(), "supavisor.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	if _, err := ParseConfigFile(path); err == nil {
		t.Error("Expected an invalid stopsignal to be rejected")
	}
}

func TestParseConfigFile_AcceptsStopSignalWithOrWithoutPrefix(t *testing.T) {
	content := `
programs:
  a:
    command: /bin/true
    stopsignal: INT
  b:
    command: /bin/true
    stopsignal: SIGQUIT
  c:
    command: /bin/true
`
	path := filepath.Join(t.TempDir(), "supavisor.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := ParseConfigFile(path)
	if err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}
	if cfg.Programs["a"].StopSignal != syscall.SIGINT {
		t.Errorf("stopsignal INT = %v", cfg.Programs["a"].StopSignal)
	}
	if cfg.Programs["b"].StopSignal != syscall.SIGQUIT {
		t.Errorf("stopsignal SIGQUIT = %v", cfg.Programs["b"].StopSignal)
	}
	if cfg.Programs["c"].StopSignal != syscall.SIGTERM {
		t.Errorf("Default stopsignal = %v, expected SIGTERM", cfg.Programs["c"].StopSignal)
	}
}

// TestParseConfigFile_RejectsUserUntilImplemented guards against a silent
// security surprise: `user: nobody` was parsed and ignored, so the program ran
// as whatever supavisor runs as, typically root.
func TestParseConfigFile_RejectsUserUntilImplemented(t *testing.T) {
	content := `
programs:
  app:
    command: /bin/true
    user: nobody
`
	path := filepath.Join(t.TempDir(), "supavisor.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	_, err := ParseConfigFile(path)
	if err == nil {
		t.Fatal("Expected user to be rejected while it has no effect")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Error should say the setting is unimplemented, got: %v", err)
	}
}

// TestValidate_RejectsSharedLogFiles guards the assumption rotation relies on:
// supavisor owns each log file, so two programs cannot share one.
func TestValidate_RejectsSharedLogFiles(t *testing.T) {
	cfg := &Config{
		Supavisor: SupavisorConfig{Socket: "/tmp/s.sock"},
		Programs: map[string]*ProgramConfig{
			"a": {Name: "a", Command: "/bin/true", StdoutLogfile: "/var/log/shared.log"},
			"b": {Name: "b", Command: "/bin/true", StdoutLogfile: "/var/log/shared.log"},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Expected two programs sharing a log file to be rejected")
	}
	if !strings.Contains(err.Error(), "shared.log") {
		t.Errorf("Error should name the file, got: %v", err)
	}
}

// TestValidate_AllowsOneProgramSharingItsOwnStreams keeps the supported case
// working: stdout and stderr of the same program may point at one file.
func TestValidate_AllowsOneProgramSharingItsOwnStreams(t *testing.T) {
	cfg := &Config{
		Supavisor: SupavisorConfig{Socket: "/tmp/s.sock"},
		Programs: map[string]*ProgramConfig{
			"a": {
				Name: "a", Command: "/bin/true",
				StdoutLogfile: "/var/log/a.log",
				StderrLogfile: "/var/log/a.log",
			},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("A program may point both streams at one file: %v", err)
	}
}

// TestValidate_RejectsAnUnbindableSocketPath turns a bare "bind: invalid
// argument" from the listener into something that says what is wrong.
func TestValidate_RejectsAnUnbindableSocketPath(t *testing.T) {
	cfg := &Config{
		Supavisor: SupavisorConfig{Socket: "/tmp/" + strings.Repeat("d/", 60) + "s.sock"},
		Programs:  map[string]*ProgramConfig{},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Expected an over-long socket path to be rejected")
	}
	if !strings.Contains(err.Error(), "unix socket") {
		t.Errorf("Error should explain the limit, got: %v", err)
	}
}
