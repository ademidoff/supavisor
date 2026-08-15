package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ademidoff/supavisor/internal/api"
	"github.com/ademidoff/supavisor/internal/version"
)

const (
	tabPadding = 3

	// States a program has a live PID in, and the placeholders used when a
	// column has nothing to report
	stateRunning  = "RUNNING"
	stateStarting = "STARTING"
	stateStopping = "STOPPING"
	healthNone    = "NONE"
	notAvailable  = "N/A"

	// dialTimeout fails fast when the daemon is not listening.
	dialTimeout = 5 * time.Second

	// requestTimeout has to outlast a command's own work: stopping a process
	// that ignores SIGINT takes the full graceful shutdown timeout before it is
	// killed, and a start waits for the process to come up.
	requestTimeout = 60 * time.Second
)

func main() {
	var socketPath string
	var showVersion bool
	flag.StringVar(&socketPath, "s", "/tmp/supavisor.sock", "Path to supavisor socket")
	flag.StringVar(&socketPath, "socket", "/tmp/supavisor.sock", "Path to supavisor socket")
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit")
	flag.Usage = printUsage
	flag.Parse()

	if showVersion {
		fmt.Println(version.String("sctl"))
		return
	}

	if flag.NArg() == 0 {
		printUsage()
		os.Exit(1)
	}

	command := flag.Arg(0)
	args := flag.Args()[1:]

	// Go's flag package stops parsing at the first non-flag argument, so an
	// option placed after the command would be silently ignored and we would
	// talk to the default socket instead of the requested one.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "Error: options must be given before the command: %s\n\n", arg)
			printUsage()
			os.Exit(1)
		}
	}

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
	if command == api.CommandStatus {
		// A named program gets the detail view, which has room for why it is
		// not running; the table stays narrow enough to read.
		if len(args) > 0 {
			printProcessDetail(*resp)
		} else {
			printStatus(*resp)
		}
		return
	}
	fmt.Println(resp.Message)
}

func printStatus(resp api.Response) {
	rows, ok := statusRows(resp)
	if !ok {
		fmt.Println(resp.Message)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, tabPadding, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tDESIRED\tHEALTH\tPID\tEXIT_CODE\tRESTARTS\tUPTIME")
	fmt.Fprintln(w, "----\t-----\t-------\t------\t---\t---------\t--------\t------")

	for _, row := range rows {
		state := getString(row, "state")

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			getString(row, "name"), state, getString(row, "desired"),
			healthColumn(getString(row, "health")), pidColumn(state, getInt(row, "pid")),
			getInt(row, "exit_code"), getInt(row, "restart_count"), getString(row, "uptime"))
	}

	_ = w.Flush()
}

// printProcessDetail reports on a single program, including why it is not
// running when it is wanted but is not
func printProcessDetail(resp api.Response) {
	rows, ok := statusRows(resp)
	if !ok || len(rows) == 0 {
		fmt.Println(resp.Message)
		return
	}
	row := rows[0]
	state := getString(row, "state")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, tabPadding, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", getString(row, "name"))
	fmt.Fprintf(w, "State:\t%s\n", state)
	fmt.Fprintf(w, "Desired:\t%s\n", getString(row, "desired"))
	fmt.Fprintf(w, "Health:\t%s\n", healthColumn(getString(row, "health")))
	fmt.Fprintf(w, "PID:\t%s\n", pidColumn(state, getInt(row, "pid")))
	fmt.Fprintf(w, "Exit code:\t%d\n", getInt(row, "exit_code"))
	fmt.Fprintf(w, "Restarts:\t%d\n", getInt(row, "restart_count"))
	fmt.Fprintf(w, "Uptime:\t%s\n", getString(row, "uptime"))

	// Only a program with something to explain has a reason: it is held back by
	// a dependency, or it stopped trying on its own.
	if reason := getString(row, "reason"); reason != "" {
		fmt.Fprintf(w, "Reason:\t%s\n", reason)
	}

	_ = w.Flush()
}

// statusRows extracts the process list from a status response
func statusRows(resp api.Response) ([]map[string]any, bool) {
	data, ok := resp.Data.(map[string]any)
	if !ok {
		return nil, false
	}
	processes, ok := data["processes"].([]any)
	if !ok {
		return nil, false
	}

	rows := make([]map[string]any, 0, len(processes))
	for _, p := range processes {
		row, ok := p.(map[string]any)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows, true
}

// healthColumn renders the health of a program. Programs without a health
// check, and programs that are not running, have nothing to report.
func healthColumn(health string) string {
	if health == "" || health == healthNone {
		return "-"
	}
	return health
}

// pidColumn renders the PID of a program that has one
func pidColumn(state string, pid int) string {
	if slices.Contains([]string{stateRunning, stateStarting, stateStopping}, state) {
		return fmt.Sprintf("%d", pid)
	}
	return notAvailable
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
		Timeout: dialTimeout,
	}
	conn, err := dialer.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to supavisor: %w\nMake sure the supavisor daemon is running", err)
	}
	defer conn.Close()

	// Set read and write deadlines to prevent hanging
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
	fmt.Println("  status <name>       Show one process in detail, including why it is not running")
	fmt.Println("  start <name>        Start a process")
	fmt.Println("  stop <name>         Stop a process")
	fmt.Println("  restart <name>      Restart a process")
	fmt.Println("  reload              Reload configuration")
	fmt.Println("  shutdown            Shutdown supavisor")
	fmt.Println()
	fmt.Println("Options (must precede the command):")
	fmt.Println("  -s, -socket PATH    Path to supavisor socket (default: /tmp/supavisor.sock)")
	fmt.Println("  -version            Print version information and exit")
}
