package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/ademidoff/go-supervisord/internal/config"
	"github.com/ademidoff/go-supervisord/internal/supervisord"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "c", "/etc/go-supervisord/supervisord.conf", "Path to configuration file")
	flag.StringVar(&configPath, "config", "/etc/go-supervisord/supervisord.conf", "Path to configuration file")
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

	// Setup supervisord's own logfile if configured
	if cfg.Supervisord.LogFile != "" {
		// Ensure log directory exists
		if dir := filepath.Dir(cfg.Supervisord.LogFile); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "Error: failed to create log directory: %v\n", err)
				os.Exit(1)
			}
		}

		// Open log file for appending
		logFile, err := os.OpenFile(cfg.Supervisord.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer logFile.Close()

		// Set log output to the file
		log.SetOutput(logFile)
		log.SetFlags(log.LstdFlags)
	}

	// Create supervisord
	sv, err := supervisord.NewSupervisor(cfg)
	if err != nil {
		log.Printf("Error: failed to create supervisord: %v\n", err)
		os.Exit(1)
	}

	// Start supervisord
	if err := sv.Start(); err != nil {
		log.Printf("Error: failed to start supervisord: %v\n", err)
		os.Exit(1)
	}

	log.Println("supervisord started successfully")

	// Wait forever (supervisord will handle signals)
	select {}
}
