package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ademidoff/go-supervisord/internal/config"
	"github.com/ademidoff/go-supervisord/internal/supervisord"
)

func main() {
	var configPath string
	var logFilePath string

	flag.StringVar(&configPath, "c", "/etc/go-supervisord/supervisord.conf", "Path to configuration file")
	flag.StringVar(&configPath, "config", "/etc/go-supervisord/supervisord.conf", "Path to configuration file")
	flag.StringVar(&logFilePath, "logfile", "", "Optional path to log file (logs always go to stdout)")
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

	// Setup logging
	var output io.Writer = os.Stdout

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

		// Set log output to both stdout and the log file
		output = io.MultiWriter(os.Stdout, logFile)
	}

	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			level := a.Value.Any().(slog.Level)
			a.Value = slog.StringValue(strings.ToLower(level.String()))
		}
		return a
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: replaceAttr,
	}

	switch cfg.Supervisord.LogFormat {
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

	// Create supervisord with main component logger
	sv, err := supervisord.NewSupervisor(cfg, logger)
	if err != nil {
		l.Error("Failed to create supervisord", "error", err)
		os.Exit(1)
	}

	// Start supervisord
	if err := sv.Start(); err != nil {
		l.Error("Failed to start supervisord", "error", err)
		os.Exit(1)
	}

	// Wait forever (supervisord will handle signals)
	select {}
}
