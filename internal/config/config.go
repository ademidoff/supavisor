package config

import (
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
)

// RestartPolicy represents the restart behavior for a process
type RestartPolicy string

const (
	RestartAlways     RestartPolicy = "always"
	RestartNever      RestartPolicy = "never"
	RestartUnexpected RestartPolicy = "unexpected"
)

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
	Name                    string
	Command                 string
	Directory               string
	Autostart               bool
	Autorestart             RestartPolicy
	StartSecs               int
	StartRetries            int
	DependsOn               []string
	StdoutLogfile           string
	StderrLogfile           string
	StdoutLogfileMaxBytes   int64
	StdoutLogfileBackups    int
	StdoutLogfileMaxAge     int // days
	StderrLogfileMaxBytes   int64
	StderrLogfileBackups    int
	StderrLogfileMaxAge     int // days
	Environment             map[string]string
	User                    string
	Priority                int
}

// Config represents the complete configuration
type Config struct {
	Supavisor SupavisorConfig
	Programs  map[string]*ProgramConfig
}

// ParseConfigFile parses an INI configuration file
func ParseConfigFile(path string) (*Config, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load config file: %w", err)
	}

	config := &Config{
		Programs: make(map[string]*ProgramConfig),
	}

	// Parse [supavisor] section
	if sec := cfg.Section("supavisor"); sec != nil {
		config.Supavisor.LogFile = sec.Key("logfile").String()
		config.Supavisor.PidFile = sec.Key("pidfile").MustString("/var/run/supavisor.pid")
		config.Supavisor.Socket = sec.Key("socket").MustString("/tmp/supavisor.sock")
		config.Supavisor.LogFormat = sec.Key("log_format").MustString("text")
		config.Supavisor.LogLevel = sec.Key("log_level").MustString("info")
	}

	// Parse [program:*] sections
	for _, section := range cfg.Sections() {
		if after, ok := strings.CutPrefix(section.Name(), "program:"); ok {
			programName := after
			programConfig, err := parseProgramSection(section, programName)
			if err != nil {
				return nil, fmt.Errorf("failed to parse program %s: %w", programName, err)
			}
			config.Programs[programName] = programConfig
		}
	}

	return config, nil
}

func parseProgramSection(section *ini.Section, name string) (*ProgramConfig, error) {
	prog := &ProgramConfig{
		Name:        name,
		Environment: make(map[string]string),
	}

	prog.Command = section.Key("command").String()
	if prog.Command == "" {
		return nil, fmt.Errorf("command is required for program %s", name)
	}

	prog.Directory = section.Key("directory").MustString("")
	prog.Autostart = section.Key("autostart").MustBool(true)

	restartPolicy := section.Key("autorestart").MustString("unexpected")
	switch RestartPolicy(restartPolicy) {
	case RestartAlways, RestartNever, RestartUnexpected:
		prog.Autorestart = RestartPolicy(restartPolicy)
	default:
		return nil, fmt.Errorf("invalid autorestart policy: %s (must be always, never, or unexpected)", restartPolicy)
	}

	prog.StartSecs = section.Key("startsecs").MustInt(1)
	prog.StartRetries = section.Key("startretries").MustInt(3)

	// Parse dependencies
	dependsOn := section.Key("depends_on").String()
	if dependsOn != "" {
		deps := strings.SplitSeq(dependsOn, ",")
		for dep := range deps {
			dep = strings.TrimSpace(dep)
			if dep != "" {
				prog.DependsOn = append(prog.DependsOn, dep)
			}
		}
	}

	// Log file settings
	prog.StdoutLogfile = section.Key("stdout_logfile").MustString("")
	prog.StderrLogfile = section.Key("stderr_logfile").MustString("")

	// Parse maxbytes (supports MB, KB, GB suffixes)
	prog.StdoutLogfileMaxBytes = parseBytes(section.Key("stdout_logfile_maxbytes").MustString("50MB"))
	prog.StdoutLogfileBackups = section.Key("stdout_logfile_backups").MustInt(10)
	prog.StdoutLogfileMaxAge = section.Key("stdout_logfile_maxage").MustInt(0) // 0 means no age limit

	prog.StderrLogfileMaxBytes = parseBytes(section.Key("stderr_logfile_maxbytes").MustString("50MB"))
	prog.StderrLogfileBackups = section.Key("stderr_logfile_backups").MustInt(10)
	prog.StderrLogfileMaxAge = section.Key("stderr_logfile_maxage").MustInt(0)

	// Parse environment variables
	envStr := section.Key("environment").String()
	if envStr != "" {
		envVars, err := parseEnvironmentVariables(envStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse environment variables: %w", err)
		}
		maps.Copy(prog.Environment, envVars)
	}

	prog.User = section.Key("user").MustString("")
	prog.Priority = section.Key("priority").MustInt(999)

	return prog, nil
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
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory %s: %w", dir, err)
		}
	}

	return nil
}

// parseEnvironmentVariables parses comma-separated environment variables
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
// Keys should not contain quotes or equals signs
// Values can be quoted to contain commas
func parseEnvPair(pair string) (string, string, error) {
	// Find the first '=' sign (keys shouldn't have quotes or special chars)
	equalsIdx := strings.Index(pair, "=")
	if equalsIdx == -1 {
		return "", "", fmt.Errorf("missing '=' in environment variable")
	}

	key := strings.TrimSpace(pair[:equalsIdx])
	value := strings.TrimSpace(pair[equalsIdx+1:])

	// Remove surrounding quotes from value if present
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
