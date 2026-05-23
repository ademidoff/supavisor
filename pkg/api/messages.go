// Package api contains the API messages for the supavisor and sctl.
package api

// Request represents a request from the CLI
type Request struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Response represents a response from the daemon
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// ProcessStatus represents the status of a process
type ProcessStatus struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	PID          int    `json:"pid"`
	ExitCode     int    `json:"exit_code"`
	RestartCount int    `json:"restart_count"`
	Uptime       string `json:"uptime"`
}

// StatusResponse represents a status response
type StatusResponse struct {
	Processes []ProcessStatus `json:"processes"`
}
