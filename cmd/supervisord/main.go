package main

import (
	"flag"
	"fmt"
	"log"
	"os"

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

	// Create supervisord
	sv, err := supervisord.NewSupervisor(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to create supervisord: %v\n", err)
		os.Exit(1)
	}

	// Start supervisord
	if err := sv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to start supervisord: %v\n", err)
		os.Exit(1)
	}

	log.Println("supervisord started successfully")

	// Wait forever (supervisord will handle signals)
	select {}
}
