package config

import (
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RestartPolicy represents the restart behavior for a process
type RestartPolicy string

const (
	RestartAlways     RestartPolicy = "always"
	RestartNever      RestartPolicy = "never"
	RestartUnexpected RestartPolicy = "unexpected"
)

const defaultLogFileMaxBytes = 50 * 1024 * 1024

// SupavisorConfig represents the main supavisor configuration
type SupavisorConfig struct {
	LogFile   string
	PidFile   string
	Socket    string
	LogFormat string
	LogLevel  string
}

// ProgramConfig represents configuration for a single program
type ProgramConfig struct {
	Environment           map[string]string
	Name                  string
	Command               string
	Directory             string
	Autorestart           RestartPolicy
	StdoutLogfile         string
	StderrLogfile         string
	User                  string
	DependsOn             []string
	Autostart             bool
	Priority              int
	StartSecs             int
	MaxRestarts           int
	StdoutLogfileMaxBytes int64
	StdoutLogfileBackups  int
	StdoutLogfileMaxAge   int // days
	StderrLogfileMaxBytes int64
	StderrLogfileBackups  int
	StderrLogfileMaxAge   int // days
}

// Config represents the complete configuration
type Config struct {
	Programs  map[string]*ProgramConfig
	Supavisor SupavisorConfig
}

// configFile represents the YAML config file structure
type configFile struct {
	Programs  map[string]*programFile `yaml:"programs"`
	Supavisor supavisorFile           `yaml:"supavisor"`
}

type supavisorFile struct {
	LogFile   string `yaml:"logfile"`
	PidFile   string `yaml:"pidfile"`
	Socket    string `yaml:"socket"`
	LogFormat string `yaml:"log_format"`
	LogLevel  string `yaml:"log_level"`
}

type programFile struct {
	Environment           map[string]string `yaml:"environment"`
	Autostart             *bool             `yaml:"autostart"`
	Command               string            `yaml:"command"`
	Directory             string            `yaml:"directory"`
	Autorestart           string            `yaml:"autorestart"`
	StdoutLogfile         string            `yaml:"stdout_logfile"`
	StderrLogfile         string            `yaml:"stderr_logfile"`
	StdoutLogfileMaxBytes string            `yaml:"stdout_logfile_maxbytes"`
	StderrLogfileMaxBytes string            `yaml:"stderr_logfile_maxbytes"`
	User                  string            `yaml:"user"`
	DependsOn             []string          `yaml:"depends_on"`
	Priority              int               `yaml:"priority"`
	StartSecs             int               `yaml:"startsecs"`
	MaxRestarts           int               `yaml:"max_restarts"`
	StdoutLogfileBackups  int               `yaml:"stdout_logfile_backups"`
	StdoutLogfileMaxAge   int               `yaml:"stdout_logfile_maxage"`
	StderrLogfileBackups  int               `yaml:"stderr_logfile_backups"`
	StderrLogfileMaxAge   int               `yaml:"stderr_logfile_maxage"`
}

// ParseConfigFile parses a YAML configuration file
func ParseConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg configFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	config := &Config{
		Supavisor: SupavisorConfig{
			LogFile:   cfg.Supavisor.LogFile,
			PidFile:   defaultString(cfg.Supavisor.PidFile, "/var/run/supavisor.pid"),
			Socket:    defaultString(cfg.Supavisor.Socket, "/tmp/supavisor.sock"),
			LogFormat: defaultString(cfg.Supavisor.LogFormat, "text"),
			LogLevel:  defaultString(cfg.Supavisor.LogLevel, "info"),
		},
		Programs: make(map[string]*ProgramConfig),
	}

	for name, prog := range cfg.Programs {
		if prog == nil {
			continue
		}
		programConfig, err := convertProgram(name, prog)
		if err != nil {
			return nil, fmt.Errorf("program %s: %w", name, err)
		}
		config.Programs[name] = programConfig
	}

	return config, nil
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func convertProgram(name string, raw *programFile) (*ProgramConfig, error) {
	if raw.Command == "" {
		return nil, fmt.Errorf("command is required")
	}

	autostart := true
	if raw.Autostart != nil {
		autostart = *raw.Autostart
	}

	restartPolicy := defaultString(raw.Autorestart, "unexpected")
	var autorestart RestartPolicy
	switch restartPolicy {
	case "always":
		autorestart = RestartAlways
	case "never":
		autorestart = RestartNever
	case "unexpected":
		autorestart = RestartUnexpected
	default:
		return nil, fmt.Errorf("invalid autorestart policy: %s (must be always, never, or unexpected)", restartPolicy)
	}

	startSecs := raw.StartSecs
	if startSecs == 0 {
		startSecs = 1
	}
	maxRestarts := raw.MaxRestarts
	if maxRestarts == 0 {
		maxRestarts = 3
	}
	priority := raw.Priority
	if priority == 0 {
		priority = 999
	}

	env := make(map[string]string)
	if len(raw.Environment) > 0 {
		maps.Copy(env, raw.Environment)
	}

	stdoutMaxBytes := parseBytes(raw.StdoutLogfileMaxBytes)
	if stdoutMaxBytes == 0 {
		stdoutMaxBytes = defaultLogFileMaxBytes
	}
	stderrMaxBytes := parseBytes(raw.StderrLogfileMaxBytes)
	if stderrMaxBytes == 0 {
		stderrMaxBytes = defaultLogFileMaxBytes
	}
	stdoutBackups := raw.StdoutLogfileBackups
	if stdoutBackups == 0 {
		stdoutBackups = 10
	}
	stderrBackups := raw.StderrLogfileBackups
	if stderrBackups == 0 {
		stderrBackups = 10
	}

	return &ProgramConfig{
		Name:                  name,
		Command:               raw.Command,
		Directory:             raw.Directory,
		Environment:           env,
		Autostart:             autostart,
		Autorestart:           autorestart,
		DependsOn:             raw.DependsOn,
		Priority:              priority,
		StartSecs:             startSecs,
		MaxRestarts:           maxRestarts,
		StdoutLogfile:         raw.StdoutLogfile,
		StderrLogfile:         raw.StderrLogfile,
		StdoutLogfileMaxBytes: stdoutMaxBytes,
		StdoutLogfileBackups:  stdoutBackups,
		StdoutLogfileMaxAge:   raw.StdoutLogfileMaxAge,
		StderrLogfileMaxBytes: stderrMaxBytes,
		StderrLogfileBackups:  stderrBackups,
		StderrLogfileMaxAge:   raw.StderrLogfileMaxAge,
		User:                  raw.User,
	}, nil
}

// parseBytes parses a byte string like "10MB", "1GB", "500KB" into bytes
func parseBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 50 * 1024 * 1024 // Default 50MB
	}

	s = strings.ToUpper(s)
	var multiplier int64 = 1

	switch {
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "B"):
		multiplier = 1
		s = strings.TrimSuffix(s, "B")
	}

	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 50 * 1024 * 1024 // Default on error
	}

	return val * multiplier
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Check for circular dependencies
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	for name := range c.Programs {
		if !visited[name] {
			if err := c.checkCircularDependency(name, visited, recStack); err != nil {
				return err
			}
		}
	}

	// Check that all dependencies exist
	for name, prog := range c.Programs {
		for _, dep := range prog.DependsOn {
			if _, exists := c.Programs[dep]; !exists {
				return fmt.Errorf("program %s depends on %s which does not exist", name, dep)
			}
		}
	}

	return nil
}

func (c *Config) checkCircularDependency(name string, visited, recStack map[string]bool) error {
	visited[name] = true
	recStack[name] = true

	prog, exists := c.Programs[name]
	if !exists {
		return nil
	}

	for _, dep := range prog.DependsOn {
		if !visited[dep] {
			if err := c.checkCircularDependency(dep, visited, recStack); err != nil {
				return err
			}
		} else if recStack[dep] {
			return fmt.Errorf("circular dependency detected: %s -> %s", name, dep)
		}
	}

	recStack[name] = false
	return nil
}

// EnsureLogDirectories creates directories for log files if they don't exist
func (c *Config) EnsureLogDirectories() error {
	dirs := make(map[string]bool)

	for _, prog := range c.Programs {
		if prog.StdoutLogfile != "" {
			dir := getDir(prog.StdoutLogfile)
			if dir != "" {
				dirs[dir] = true
			}
		}
		if prog.StderrLogfile != "" {
			dir := getDir(prog.StderrLogfile)
			if dir != "" {
				dirs[dir] = true
			}
		}
	}

	// Create supavisor log directory
	if c.Supavisor.LogFile != "" {
		dir := getDir(c.Supavisor.LogFile)
		if dir != "" {
			dirs[dir] = true
		}
	}

	for dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create log directory %s: %w", dir, err)
		}
	}

	return nil
}

// parseEnvironmentVariables parses comma-separated environment variables (legacy INI format)
// Supports formats like: KEY1=value1,KEY2=value2,KEY3="value with, comma"
func parseEnvironmentVariables(envStr string) (map[string]string, error) {
	result := make(map[string]string)

	// Track if we're inside quotes
	inQuotes := false
	quoteChar := byte(0)
	current := ""

	for i := 0; i < len(envStr); i++ {
		char := envStr[i]

		// Handle quotes
		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			} else {
				current += string(char)
			}
		} else if char == ',' && !inQuotes {
			// End of current variable, parse it
			if current != "" {
				key, value, err := parseEnvPair(strings.TrimSpace(current))
				if err != nil {
					return nil, fmt.Errorf("invalid environment variable format '%s': %w", current, err)
				}
				result[key] = value
				current = ""
			}
		} else {
			current += string(char)
		}
	}

	// Parse the last variable
	if current != "" {
		key, value, err := parseEnvPair(strings.TrimSpace(current))
		if err != nil {
			return nil, fmt.Errorf("invalid environment variable format '%s': %w", current, err)
		}
		result[key] = value
	}

	return result, nil
}

// parseEnvPair parses a single KEY=VALUE pair
func parseEnvPair(pair string) (key, value string, err error) {
	before, after, ok := strings.Cut(pair, "=")
	if !ok {
		return "", "", fmt.Errorf("missing '=' in environment variable")
	}

	key = strings.TrimSpace(before)
	value = strings.TrimSpace(after)

	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}

	if key == "" {
		return "", "", fmt.Errorf("empty key in environment variable")
	}

	return key, value, nil
}

func getDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return ""
	}
	return path[:idx]
}
