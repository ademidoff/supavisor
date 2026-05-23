package supavisor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"

	"github.com/ademidoff/supavisor/pkg/api"
)

const msgProcessNameRequired = "process name required"

// IPCServer handles communication with the CLI tool
type IPCServer struct {
	listener   net.Listener
	supavisor  *Supavisor
	stopChan   chan struct{}
	socketPath string
}

// NewIPCServer creates a new IPC server
func NewIPCServer(socketPath string, supavisor *Supavisor) *IPCServer {
	return &IPCServer{
		socketPath: socketPath,
		supavisor:  supavisor,
		stopChan:   make(chan struct{}),
	}
}

// Start starts the IPC server
func (s *IPCServer) Start() error {
	// Remove existing socket if it exists
	if _, err := os.Stat(s.socketPath); err == nil {
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("failed to remove existing socket: %w", err)
		}
	}

	listener, err := net.Listen("unix", s.socketPath) //nolint:noctx
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	s.listener = listener

	// Set socket permissions
	if err := os.Chmod(s.socketPath, 0o666); err != nil {
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	go s.acceptConnections()

	return nil
}

// Stop stops the IPC server
func (s *IPCServer) Stop() error {
	close(s.stopChan)
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// acceptConnections accepts incoming connections
func (s *IPCServer) acceptConnections() {
	for {
		select {
		case <-s.stopChan:
			return
		default:
			conn, err := s.listener.Accept()
			if err != nil {
				select {
				case <-s.stopChan:
					return
				default:
					continue
				}
			}
			go s.handleConnection(conn)
		}
	}
}

// handleConnection handles a single connection
func (s *IPCServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req api.Request
		if err := decoder.Decode(&req); err != nil {
			break
		}

		resp := s.handleRequest(&req)
		if err := encoder.Encode(resp); err != nil {
			break
		}
	}
}

// handleRequest handles a request and returns a response
func (s *IPCServer) handleRequest(req *api.Request) *api.Response {
	switch req.Command {
	case "status":
		return s.handleStatus()
	case "start":
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleStart(req.Args[0])
	case "stop":
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleStop(req.Args[0])
	case "restart":
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleRestart(req.Args[0])
	case "reload":
		return s.handleReload()
	case "shutdown":
		return s.handleShutdown()
	default:
		return &api.Response{Success: false, Message: fmt.Sprintf("unknown command: %s", req.Command)}
	}
}

// handleStatus returns the status of all processes
func (s *IPCServer) handleStatus() *api.Response {
	statuses := s.supavisor.GetStatus()
	processStatuses := make([]api.ProcessStatus, 0, len(statuses))

	for _, status := range statuses {
		processStatuses = append(processStatuses, api.ProcessStatus{
			Name:         status.Name,
			State:        string(status.State),
			PID:          status.PID,
			ExitCode:     status.ExitCode,
			RestartCount: status.RestartCount,
			Uptime:       status.Uptime,
		})
	}

	return &api.Response{
		Success: true,
		Data:    map[string]interface{}{"processes": processStatuses},
	}
}

// handleStart starts a process
func (s *IPCServer) handleStart(name string) *api.Response {
	if err := s.supavisor.StartProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s started", name)}
}

// handleStop stops a process
func (s *IPCServer) handleStop(name string) *api.Response {
	if err := s.supavisor.StopProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s stopped", name)}
}

// handleRestart restarts a process
func (s *IPCServer) handleRestart(name string) *api.Response {
	if err := s.supavisor.RestartProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s restarted", name)}
}

// handleReload reloads the configuration
func (s *IPCServer) handleReload() *api.Response {
	if err := s.supavisor.Reload(); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: "configuration reloaded"}
}

// handleShutdown shuts down the supavisor
func (s *IPCServer) handleShutdown() *api.Response {
	go func() {
		// Send SIGTERM to ourselves
		pid := os.Getpid()
		proc, _ := os.FindProcess(pid)
		err := proc.Signal(syscall.SIGTERM)
		if err != nil {
			slog.Error("failed to send SIGTERM", "error", err)
		}
	}()
	return &api.Response{Success: true, Message: "shutdown initiated"}
}
