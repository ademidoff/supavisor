package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/ademidoff/supavisor/pkg/api"
)

const (
	tabPadding     = 3
	requestTimeout = 5 * time.Second
)

func main() {
	var socketPath string
	flag.StringVar(&socketPath, "s", "/tmp/supavisor.sock", "Path to supavisor socket")
	flag.StringVar(&socketPath, "socket", "/tmp/supavisor.sock", "Path to supavisor socket")
	flag.Usage = printUsage
	flag.Parse()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	resp, err := sendRequest(socketPath, command, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: %v\n", err)
		os.Exit(1)
	}

	// Handle response
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Message)
		os.Exit(1)
	}

	// Print response based on command
	switch command {
	case "status":
		printStatus(*resp)
	case "start", "stop", "restart", "reload", "shutdown":
		fmt.Println(resp.Message)
	default:
		fmt.Println(resp.Message)
	}
}

func printStatus(resp api.Response) {
	data, ok := resp.Data.(map[string]any)
	if !ok {
		fmt.Println(resp.Message)
		return
	}

	processesData, ok := data["processes"].([]interface{})
	if !ok {
		fmt.Println(resp.Message)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, tabPadding, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tPID\tEXIT_CODE\tRESTARTS\tUPTIME")
	fmt.Fprintln(w, "----\t-----\t---\t---------\t--------\t------")

	for _, p := range processesData {
		procMap, ok := p.(map[string]interface{})
		if !ok {
			continue
		}

		name := getString(procMap, "name")
		state := getString(procMap, "state")
		pid := getInt(procMap, "pid")
		exitCode := getInt(procMap, "exit_code")
		restarts := getInt(procMap, "restart_count")
		uptime := getString(procMap, "uptime")

		pidStr := "N/A"
		if slices.Contains([]string{"RUNNING", "STARTING", "STOPPING"}, state) {
			pidStr = fmt.Sprintf("%d", pid)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n", name, state, pidStr, exitCode, restarts, uptime)
	}

	_ = w.Flush()
}

func getString(m map[string]any, key string) string {
	val, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := val.(string)
	if !ok {
		return ""
	}
	return s
}

func getInt(m map[string]any, key string) int {
	val, ok := m[key]
	if !ok {
		return 0
	}

	// JSON numbers are float64
	f, ok := val.(float64)
	if ok {
		return int(f)
	}

	// Try int directly
	i, ok := val.(int)
	if ok {
		return i
	}

	return 0
}

// sendRequest connects to supavisor, sends a request, and returns the response.
// It includes timeouts to prevent hanging when the daemon is not running.
func sendRequest(socketPath, command string, args []string) (*api.Response, error) {
	// Connect to supavisor with timeout
	dialer := net.Dialer{
		Timeout: requestTimeout,
	}
	conn, err := dialer.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to supavisor: %w\nMake sure the supavisor daemon is running", err)
	}
	defer conn.Close()

	// Set read and write deadlines to prevent hanging
	// Use a shorter timeout for faster failure when daemon is not responding
	deadline := time.Now().Add(requestTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("failed to set connection deadline: %w", err)
	}

	// Send request
	req := api.Request{
		Command: command,
		Args:    args,
	}

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(&req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Receive response
	decoder := json.NewDecoder(conn)
	var resp api.Response
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to receive response: %w\nMake sure the supavisor daemon is running and responding", err)
	}

	return &resp, nil
}

func printUsage() {
	fmt.Println("Usage: sctl [OPTIONS] COMMAND [ARGS]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  status              Show status of all processes")
	fmt.Println("  start <name>        Start a process")
	fmt.Println("  stop <name>         Stop a process")
	fmt.Println("  restart <name>      Restart a process")
	fmt.Println("  reload              Reload configuration")
	fmt.Println("  shutdown            Shutdown supavisor")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -s, -socket PATH    Path to supavisor socket (default: /tmp/supavisor.sock)")
}
