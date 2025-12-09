package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ademidoff/go-supervisord/internal/config"
	"github.com/ademidoff/go-supervisord/internal/supervisord"
)

var logFile *os.File

// logError writes an error message to both stderr and the log file (if configured)
func logError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprint(os.Stderr, msg)
	if logFile != nil {
		fmt.Fprint(logFile, msg)
	}
}

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

	// Setup logging: always to stdout, optionally to file if flag is set
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	if logFilePath != "" {
		// Ensure log directory exists
		if dir := filepath.Dir(logFilePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("Error: failed to create log directory: %v\n", err)
				os.Exit(1)
			}
		}

		// Open log file for appending
		var err error
		logFile, err = os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("Error: failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer logFile.Close()

		// Set log output to both stdout and the log file
		multiWriter := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(multiWriter)
	}

	// Create supervisord
	sv, err := supervisord.NewSupervisor(cfg)
	if err != nil {
		// Use log.Printf which goes to both stdout and logfile (if configured)
		log.Printf("Error: failed to create supervisord: %v\n", err)
		os.Exit(1)
	}

	// Start supervisord
	if err := sv.Start(); err != nil {
		// Use log.Printf which goes to both stdout and logfile (if configured)
		log.Printf("Error: failed to start supervisord: %v\n", err)
		os.Exit(1)
	}

	log.Println("supervisord started successfully")

	// Wait forever (supervisord will handle signals)
	select {}
}
