// Package api contains the API messages for the supavisor and sctl.
package api

// Commands understood by the daemon. Both sides of the socket share these so
// the protocol is defined in one place.
const (
	CommandStatus   = "status"
	CommandStart    = "start"
	CommandStop     = "stop"
	CommandRestart  = "restart"
	CommandReload   = "reload"
	CommandShutdown = "shutdown"
)

// Request represents a request from the CLI
type Request struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Response represents a response from the daemon
type Response struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message,omitempty"`
	Success bool        `json:"success"`
}

// ProcessStatus represents the status of a process
type ProcessStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Desired string `json:"desired"`
	Health  string `json:"health"`
	Uptime  string `json:"uptime"`

	// Reason is why a program that is wanted is not running yet, and is absent
	// whenever nothing is holding it back.
	Reason       string `json:"reason,omitempty"`
	PID          int    `json:"pid"`
	ExitCode     int    `json:"exit_code"`
	RestartCount int    `json:"restart_count"`
}

// StatusResponse represents a status response
type StatusResponse struct {
	Processes []ProcessStatus `json:"processes"`
}

// ReloadResponse reports what a reload did. A caller driving supavisor over the
// socket needs this to tell an applied change from a no-op, and a program that
// reloads its own supervisor needs it to know whether it was in Changed and is
// therefore about to be stopped and replaced.
type ReloadResponse struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// Empty reports whether the reload left every program alone.
func (r ReloadResponse) Empty() bool {
	return len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Changed) == 0
}
