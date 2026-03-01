package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ademidoff/supavisor/internal/config"
	"github.com/ademidoff/supavisor/internal/supavisor"
	"golang.org/x/term"
)

// parseLogLevel converts a string log level to slog.Level
func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %s: must be debug, info, warn, or error", level)
	}
}

func main() {
	var configPath string
	var logFilePath string

	flag.StringVar(&configPath, "c", "/etc/supavisor/supavisor.yml", "Path to configuration file")
	flag.StringVar(&configPath, "config", "/etc/supavisor/supavisor.yml", "Path to configuration file")
	flag.StringVar(&logFilePath, "logfile", "", "Optional path to log file (logs go to stdout only in interactive mode)")
	flag.Parse()

	if configPath == "" {
		fmt.Fprintf(os.Stderr, "Error: configuration file path is required\n")
		os.Exit(1)
	}

	// Parse configuration
	cfg, err := config.ParseConfigFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to parse configuration: %v\n", err)
		os.Exit(1)
	}

	// Use logfile from config if not specified via flag
	if logFilePath == "" && cfg.Supavisor.LogFile != "" {
		logFilePath = cfg.Supavisor.LogFile
	}

	// Setup logging
	// Detect if stdout is a TTY (interactive terminal)
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	var output io.Writer

	// If a log file is specified, always use it (and only it)
	// This is standard daemon behavior - logs go to the file, not stdout
	if logFilePath != "" {
		// Ensure log directory exists
		if dir := filepath.Dir(logFilePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil { //nolint:govet
				fmt.Fprintf(os.Stderr, "Error: failed to create log directory: %v\n", err)
				os.Exit(1)
			}
		}

		// Open log file for appending
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) //nolint:govet
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to open log file: %v\n", err)
			os.Exit(1)
		}
		// We don't close logFile here because it needs to stay open for the logger
		// In a real daemon, we might want to handle rotation or closure on exit,
		// but main() exit closes files anyway.

		// Write logs only to the file (standard daemon behavior)
		output = logFile
	} else if isTTY {
		// No logfile specified, use stdout only in interactive mode
		output = os.Stdout
	} else {
		// No logfile and non-interactive mode, discard logs
		output = io.Discard
	}

	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			level := a.Value.Any().(slog.Level)
			a.Value = slog.StringValue(strings.ToLower(level.String()))
		}
		return a
	}

	// Parse log level
	logLevel, err := parseLogLevel(cfg.Supavisor.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level:       logLevel,
		ReplaceAttr: replaceAttr,
	}

	switch cfg.Supavisor.LogFormat {
	case "json":
		handler = slog.NewJSONHandler(output, opts)
	default:
		handler = slog.NewTextHandler(output, opts)
	}

	// Create a logger with setup component
	logger := slog.New(handler)
	slog.SetDefault(logger)

	l := logger.With("component", "setup")
	l.Info("Setup completed.")

	// Create supavisor with main component logger
	sv, err := supavisor.NewSupavisor(cfg, logger)
	if err != nil {
		l.Error("Failed to create supavisor", "error", err)
		os.Exit(1)
	}

	// Start supavisor
	if err := sv.Start(); err != nil {
		l.Error("Failed to start supavisor", "error", err)
		os.Exit(1)
	}

	// Wait forever (supavisor will handle signals)
	select {}
}
