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
	Name         string `json:"name"`
	State        string `json:"state"`
	Uptime       string `json:"uptime"`
	PID          int    `json:"pid"`
	ExitCode     int    `json:"exit_code"`
	RestartCount int    `json:"restart_count"`
}

// StatusResponse represents a status response
type StatusResponse struct {
	Processes []ProcessStatus `json:"processes"`
}
