package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// RestartPolicy represents the restart behavior for a process
type RestartPolicy string

const (
	RestartAlways     RestartPolicy = "always"
	RestartNever      RestartPolicy = "never"
	RestartUnexpected RestartPolicy = "unexpected"
)

const (
	defaultLogFileMaxBytes = 50 * 1024 * 1024

	// defaultStopWaitSecs is how long a process gets to exit on its stop signal
	// before it is killed.
	defaultStopWaitSecs = 10

	defaultLogFileBackups = 10
	defaultStartSecs      = 1
	defaultMaxRestarts    = 3
	defaultPriority       = 999

	// maxSocketPathLen is the smallest sockaddr_un limit across the platforms
	// supavisor runs on (104 on macOS, 108 on Linux), including the terminator.
	maxSocketPathLen = 104
)

// intOrDefault returns the configured value, treating an absent setting rather
// than a zero one as "not configured".
func intOrDefault(configured *int, fallback int) int {
	if configured == nil {
		return fallback
	}
	return *configured
}

// SupavisorConfig represents the main supavisor configuration
type SupavisorConfig struct {
	LogFile     string
	PidFile     string
	Socket      string
	SocketGroup string
	LogFormat   string
	LogLevel    string
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
	StopSignal            syscall.Signal
	StopWaitSecs          int
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
	Programs map[string]*ProgramConfig
	// SourcePath is the main config file this was read from, so that a reload
	// knows where to look. It is empty for a configuration built in code.
	SourcePath string
	Supavisor  SupavisorConfig
}

// configFile represents the YAML config file structure
type configFile struct {
	Programs  map[string]*programFile `yaml:"programs"`
	Supavisor supavisorFile           `yaml:"supavisor"`
}

type supavisorFile struct {
	LogFile     string `yaml:"logfile"`
	PidFile     string `yaml:"pidfile"`
	Socket      string `yaml:"socket"`
	SocketGroup string `yaml:"socket_group"`
	LogFormat   string `yaml:"log_format"`
	LogLevel    string `yaml:"log_level"`
}

type programFile struct {
	Environment map[string]string `yaml:"environment"`
	Autostart   *bool             `yaml:"autostart"`

	// Pointers so an explicit 0 is distinguishable from an absent setting:
	// max_restarts: 0 means never retry, not "use the default of 3".
	Priority             *int `yaml:"priority"`
	StartSecs            *int `yaml:"startsecs"`
	StopWaitSecs         *int `yaml:"stopwaitsecs"`
	MaxRestarts          *int `yaml:"max_restarts"`
	StdoutLogfileBackups *int `yaml:"stdout_logfile_backups"`
	StdoutLogfileMaxAge  *int `yaml:"stdout_logfile_maxage"`
	StderrLogfileBackups *int `yaml:"stderr_logfile_backups"`
	StderrLogfileMaxAge  *int `yaml:"stderr_logfile_maxage"`

	Command               string   `yaml:"command"`
	Directory             string   `yaml:"directory"`
	Autorestart           string   `yaml:"autorestart"`
	StopSignal            string   `yaml:"stopsignal"`
	StdoutLogfile         string   `yaml:"stdout_logfile"`
	StderrLogfile         string   `yaml:"stderr_logfile"`
	StdoutLogfileMaxBytes string   `yaml:"stdout_logfile_maxbytes"`
	StderrLogfileMaxBytes string   `yaml:"stderr_logfile_maxbytes"`
	User                  string   `yaml:"user"`
	DependsOn             []string `yaml:"depends_on"`
}

// ParseConfigFile parses a single YAML configuration file. It does not look for
// fragment files. Use ParseConfig for the full main-file + supavisor.d/ behavior.
func ParseConfigFile(path string) (*Config, error) {
	cfg, err := parseConfigFileRaw(path)
	if err != nil {
		return nil, err
	}

	config := &Config{
		Supavisor: SupavisorConfig{
			LogFile:     cfg.Supavisor.LogFile,
			PidFile:     defaultString(cfg.Supavisor.PidFile, "/var/run/supavisor.pid"),
			Socket:      defaultString(cfg.Supavisor.Socket, "/tmp/supavisor.sock"),
			SocketGroup: cfg.Supavisor.SocketGroup,
			LogFormat:   defaultString(cfg.Supavisor.LogFormat, "text"),
			LogLevel:    defaultString(cfg.Supavisor.LogLevel, "info"),
		},
		Programs: make(map[string]*ProgramConfig),
	}

	config.SourcePath = path

	if err := mergePrograms(config.Programs, cfg.Programs, path, map[string]string{}); err != nil {
		return nil, err
	}

	return config, nil
}

// ParseConfig parses the main config file and merges any fragment files found
// in the sibling directory <basename-no-ext>.d/ (e.g. /etc/supavisor/supavisor.yml
// -> /etc/supavisor/supavisor.d/). Fragments are loaded in lexical order and may
// only define the programs section. Duplicate program names across files are a
// hard error.
func ParseConfig(mainPath string) (*Config, error) {
	cfg, err := ParseConfigFile(mainPath)
	if err != nil {
		return nil, err
	}

	dropDir := fragmentDir(mainPath)
	fragments, err := listFragmentFiles(dropDir)
	if err != nil {
		return nil, err
	}

	origins := make(map[string]string, len(cfg.Programs))
	for name := range cfg.Programs {
		origins[name] = mainPath
	}

	for _, fragPath := range fragments {
		frag, err := parseConfigFileRaw(fragPath)
		if err != nil {
			return nil, err
		}
		if !isSupavisorSectionEmpty(&frag.Supavisor) {
			return nil, fmt.Errorf("fragment %s: must not define a supavisor section; daemon settings belong in the main config file", fragPath)
		}
		if err := mergePrograms(cfg.Programs, frag.Programs, fragPath, origins); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// fragmentDir returns the sibling drop-in directory for a given main config path.
// For /etc/supavisor/supavisor.yml it returns /etc/supavisor/supavisor.d.
func fragmentDir(mainPath string) string {
	dir := filepath.Dir(mainPath)
	base := filepath.Base(mainPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, stem+".d")
}

// listFragmentFiles returns *.yml and *.yaml files in dir sorted lexically.
// A missing directory is not an error.
func listFragmentFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read fragment directory %s: %w", dir, err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func parseConfigFileRaw(path string) (*configFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	// Reject unknown keys rather than ignoring them: a typo in a setting that
	// governs restarts or log rotation would otherwise be silently replaced by
	// a default, and the program would run with behavior nobody asked for.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var cfg configFile
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	return &cfg, nil
}

func isSupavisorSectionEmpty(s *supavisorFile) bool {
	return *s == (supavisorFile{})
}

// mergePrograms converts and merges raw program entries into dst. origins tracks
// which file each program was first defined in so duplicate errors can name both
// sources. A nil origins map disables tracking (single-file callers).
func mergePrograms(dst map[string]*ProgramConfig, src map[string]*programFile, srcPath string, origins map[string]string) error {
	for name, prog := range src {
		if prog == nil {
			continue
		}
		if existingPath, exists := origins[name]; exists {
			return fmt.Errorf("duplicate program %s: defined in %s and %s", name, existingPath, srcPath)
		}
		programConfig, err := convertProgram(name, prog)
		if err != nil {
			return fmt.Errorf("program %s (%s): %w", name, srcPath, err)
		}
		dst[name] = programConfig
		if origins != nil {
			origins[name] = srcPath
		}
	}
	return nil
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

	// Accepting this and running the program as the daemon's user anyway would
	// be a silent security surprise: a config asking for an unprivileged user
	// would run as root.
	if strings.TrimSpace(raw.User) != "" {
		return nil, fmt.Errorf("user is not implemented: remove it, or run supavisor as %s", raw.User)
	}

	stopSignal, err := parseSignal(raw.StopSignal)
	if err != nil {
		return nil, err
	}

	startSecs := intOrDefault(raw.StartSecs, defaultStartSecs)
	stopWaitSecs := intOrDefault(raw.StopWaitSecs, defaultStopWaitSecs)
	maxRestarts := intOrDefault(raw.MaxRestarts, defaultMaxRestarts)
	priority := intOrDefault(raw.Priority, defaultPriority)

	for field, value := range map[string]int{
		"startsecs": startSecs, "stopwaitsecs": stopWaitSecs, "max_restarts": maxRestarts,
	} {
		if value < 0 {
			return nil, fmt.Errorf("invalid %s: %d (must not be negative)", field, value)
		}
	}

	env := make(map[string]string)
	if len(raw.Environment) > 0 {
		maps.Copy(env, raw.Environment)
	}

	logging, err := convertLogging(raw)
	if err != nil {
		return nil, err
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
		StopSignal:            stopSignal,
		StopWaitSecs:          stopWaitSecs,
		MaxRestarts:           maxRestarts,
		StdoutLogfile:         raw.StdoutLogfile,
		StderrLogfile:         raw.StderrLogfile,
		StdoutLogfileMaxBytes: logging.stdoutMaxBytes,
		StdoutLogfileBackups:  logging.stdoutBackups,
		StdoutLogfileMaxAge:   intOrDefault(raw.StdoutLogfileMaxAge, 0),
		StderrLogfileMaxBytes: logging.stderrMaxBytes,
		StderrLogfileBackups:  logging.stderrBackups,
		StderrLogfileMaxAge:   intOrDefault(raw.StderrLogfileMaxAge, 0),
		User:                  raw.User,
	}, nil
}

// loggingDefaults holds the resolved log rotation settings for a program
type loggingDefaults struct {
	stdoutMaxBytes int64
	stderrMaxBytes int64
	stdoutBackups  int
	stderrBackups  int
}

func convertLogging(raw *programFile) (loggingDefaults, error) {
	stdoutMaxBytes, err := parseBytes("stdout_logfile_maxbytes", raw.StdoutLogfileMaxBytes)
	if err != nil {
		return loggingDefaults{}, err
	}
	stderrMaxBytes, err := parseBytes("stderr_logfile_maxbytes", raw.StderrLogfileMaxBytes)
	if err != nil {
		return loggingDefaults{}, err
	}

	l := loggingDefaults{
		stdoutMaxBytes: stdoutMaxBytes,
		stderrMaxBytes: stderrMaxBytes,
		stdoutBackups:  intOrDefault(raw.StdoutLogfileBackups, defaultLogFileBackups),
		stderrBackups:  intOrDefault(raw.StderrLogfileBackups, defaultLogFileBackups),
	}

	return l, nil
}

// stopSignals are the signals a program may be configured to stop on
var stopSignals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"HUP":  syscall.SIGHUP,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
	"KILL": syscall.SIGKILL,
}

// parseSignal resolves a configured stop signal name, with or without the SIG
// prefix. An empty name means SIGTERM, which is what a daemon expects to be
// asked to shut down with.
func parseSignal(name string) (syscall.Signal, error) {
	if strings.TrimSpace(name) == "" {
		return syscall.SIGTERM, nil
	}

	key := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "SIG")
	sig, ok := stopSignals[key]
	if !ok {
		names := make([]string, 0, len(stopSignals))
		for known := range stopSignals {
			names = append(names, known)
		}
		sort.Strings(names)
		return 0, fmt.Errorf("invalid stopsignal: %s (must be one of %s)", name, strings.Join(names, ", "))
	}
	return sig, nil
}

// parseBytes parses a byte string like "10MB", "1GB", "500KB" into bytes. An
// empty value takes the default; anything unparseable is an error rather than a
// silent fallback, so that a typo in a size limit cannot quietly become 50MB.
func parseBytes(field, value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultLogFileMaxBytes, nil
	}

	upper := strings.ToUpper(trimmed)
	var multiplier int64 = 1

	switch {
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1024
		upper = strings.TrimSuffix(upper, "KB")
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		upper = strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1024 * 1024 * 1024
		upper = strings.TrimSuffix(upper, "GB")
	case strings.HasSuffix(upper, "B"):
		upper = strings.TrimSuffix(upper, "B")
	}

	val, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %s (expected a byte count, optionally suffixed with KB, MB or GB)", field, value)
	}
	if val < 0 {
		return 0, fmt.Errorf("invalid %s: %s (must not be negative)", field, value)
	}

	return val * multiplier, nil
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

	if err := c.validateLogPaths(); err != nil {
		return err
	}

	return c.validateSocketPath()
}

// validateLogPaths rejects two programs writing to the same log file.
//
// Supavisor owns the log descriptor so that rotation works, which means two
// programs sharing a path would rotate the same files independently and
// destroy each other's output.
func (c *Config) validateLogPaths() error {
	owners := make(map[string]string)

	for _, name := range sortedProgramNames(c.Programs) {
		prog := c.Programs[name]
		for _, path := range []string{prog.StdoutLogfile, prog.StderrLogfile} {
			if path == "" {
				continue
			}
			// One program pointing both its streams at one file is fine: they
			// share a single writer.
			if owner, taken := owners[path]; taken && owner != name {
				return fmt.Errorf("programs %s and %s both log to %s: each log file must belong to one program", owner, name, path)
			}
			owners[path] = name
		}
	}

	return nil
}

// validateSocketPath rejects a socket path the kernel cannot bind.
//
// The sockaddr_un limit is around 104 bytes, and exceeding it surfaces as a
// bare "bind: invalid argument" from the listener with nothing to point at.
func (c *Config) validateSocketPath() error {
	if len(c.Supavisor.Socket) >= maxSocketPathLen {
		return fmt.Errorf("socket path is %d bytes, which exceeds the %d byte limit for a unix socket: %s",
			len(c.Supavisor.Socket), maxSocketPathLen-1, c.Supavisor.Socket)
	}
	return nil
}

func sortedProgramNames(programs map[string]*ProgramConfig) []string {
	names := make([]string, 0, len(programs))
	for name := range programs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

func getDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return ""
	}
	return path[:idx]
}
