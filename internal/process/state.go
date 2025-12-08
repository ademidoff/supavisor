package process

// State represents the state of a process
type State string

const (
	StateStopped  State = "STOPPED"
	StateStarting State = "STARTING"
	StateRunning  State = "RUNNING"
	StateBackoff  State = "BACKOFF" // Failed to start, waiting before retry
	StateStopping State = "STOPPING"
	StateExited   State = "EXITED"
	StateFatal    State = "FATAL" // Failed to start after all retries
	StateUnknown  State = "UNKNOWN"
)

// String returns the string representation of the state
func (s State) String() string {
	return string(s)
}

// IsRunning returns true if the process is in a running state
func (s State) IsRunning() bool {
	return s == StateRunning
}

// IsStopped returns true if the process is stopped
func (s State) IsStopped() bool {
	return s == StateStopped || s == StateExited || s == StateFatal
}
