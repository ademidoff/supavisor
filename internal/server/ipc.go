package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ademidoff/supavisor/internal/api"
)

const (
	msgProcessNameRequired = "process name required"

	// socketMode keeps the control socket off-limits to arbitrary local users.
	// Anyone who can write to it can stop supervised processes and, through
	// start, cause configured commands to run as the daemon's user.
	socketMode = 0o660

	// maxConnections bounds how many clients can occupy the daemon at once, so
	// a local process cannot exhaust its goroutines or descriptors.
	maxConnections = 32

	// maxRequestBytes bounds a single request. Without it a client could stream
	// an unbounded body into the decoder and exhaust memory.
	maxRequestBytes = 64 * 1024

	// connectionTimeout drops a client that connects and then does nothing,
	// rather than holding the slot open indefinitely.
	connectionTimeout = 2 * time.Minute

	// acceptRetryDelay backs off after an accept error that is not fatal, such
	// as running out of descriptors. Retrying flat out would spin on the CPU.
	acceptRetryDelay = 50 * time.Millisecond
)

// IPCServer handles communication with the CLI tool
type IPCServer struct {
	listener    net.Listener
	server      *Server
	stopChan    chan struct{}
	slots       chan struct{}
	socketInfo  os.FileInfo
	socketPath  string
	socketGroup string
	connections sync.WaitGroup
}

// NewIPCServer creates a new IPC server
func NewIPCServer(socketPath, socketGroup string, server *Server) *IPCServer {
	return &IPCServer{
		socketPath:  socketPath,
		socketGroup: socketGroup,
		server:      server,
		stopChan:    make(chan struct{}),
		slots:       make(chan struct{}, maxConnections),
	}
}

// Start starts the IPC server
func (s *IPCServer) Start() error {
	// Safe to clear a leftover socket: the caller holds the PID file lock, so
	// no other daemon can be listening on this path.
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath) //nolint:noctx
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	// Unlink on our own terms. Go removes the socket path on Close even when a
	// newer daemon has already replaced it, which would leave that daemon
	// listening on a socket nothing can reach.
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	s.listener = listener

	info, err := os.Stat(s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to stat socket: %w", err)
	}
	s.socketInfo = info

	if err := s.restrictSocket(); err != nil {
		return err
	}

	go s.acceptConnections()

	return nil
}

// restrictSocket narrows who can talk to the daemon
func (s *IPCServer) restrictSocket() error {
	if s.socketGroup != "" {
		gid, err := lookupGroupID(s.socketGroup)
		if err != nil {
			return err
		}
		if err := os.Chown(s.socketPath, -1, gid); err != nil {
			return fmt.Errorf("failed to give socket to group %s: %w", s.socketGroup, err)
		}
	}

	if err := os.Chmod(s.socketPath, socketMode); err != nil {
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}
	return nil
}

// lookupGroupID resolves a group name or numeric id
func lookupGroupID(group string) (int, error) {
	if gid, err := strconv.Atoi(group); err == nil {
		return gid, nil
	}

	grp, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("failed to look up socket_group %s: %w", group, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %s has an unusable gid %s: %w", group, grp.Gid, err)
	}
	return gid, nil
}

// Stop stops the IPC server
func (s *IPCServer) Stop() error {
	close(s.stopChan)
	if s.listener == nil {
		return nil
	}

	err := s.listener.Close()
	s.connections.Wait()
	if s.socketInfo != nil {
		if rmErr := removeIfSame(s.socketPath, s.socketInfo); rmErr != nil {
			slog.Warn("failed to remove socket", "path", s.socketPath, "error", rmErr)
		}
	}
	return err
}

// acceptConnections accepts incoming connections
func (s *IPCServer) acceptConnections() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return
			default:
			}
			// Retrying immediately on a persistent error, such as running out
			// of descriptors, would spin on the CPU for as long as it lasts.
			slog.Warn("failed to accept connection", "error", err)
			time.Sleep(acceptRetryDelay)
			continue
		}

		// Hold the client rather than dropping it, so that a burst queues
		// instead of failing, but never run more than maxConnections at once.
		select {
		case s.slots <- struct{}{}:
		case <-s.stopChan:
			_ = conn.Close()
			return
		}

		s.connections.Add(1)
		go func() {
			defer func() {
				<-s.slots
				s.connections.Done()
			}()
			s.handleConnection(conn)
		}()
	}
}

// handleConnection handles a single connection
func (s *IPCServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	// A client that connects and then goes quiet must not hold its slot for
	// ever. The deadline is extended for each request it actually sends.
	encoder := json.NewEncoder(conn)

	for {
		if err := conn.SetDeadline(time.Now().Add(connectionTimeout)); err != nil {
			break
		}

		// A new decoder per request keeps the size limit per request rather
		// than over the lifetime of the connection.
		decoder := json.NewDecoder(io.LimitReader(conn, maxRequestBytes))

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
	case api.CommandStatus:
		return s.handleStatus(req.Args)
	case api.CommandStart:
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleStart(req.Args[0])
	case api.CommandStop:
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleStop(req.Args[0])
	case api.CommandRestart:
		if len(req.Args) == 0 {
			return &api.Response{Success: false, Message: msgProcessNameRequired}
		}
		return s.handleRestart(req.Args[0])
	case api.CommandReload:
		return s.handleReload()
	case api.CommandShutdown:
		return s.handleShutdown()
	default:
		return &api.Response{Success: false, Message: fmt.Sprintf("unknown command: %s", req.Command)}
	}
}

// handleStatus returns the status of every process, or of one named process
func (s *IPCServer) handleStatus(args []string) *api.Response {
	statuses := s.server.GetStatus()

	if len(args) > 0 {
		statuses = selectProcess(statuses, args[0])
		if len(statuses) == 0 {
			return &api.Response{Success: false, Message: fmt.Sprintf("process %s not found", args[0])}
		}
	}

	processStatuses := make([]api.ProcessStatus, 0, len(statuses))
	for _, status := range statuses {
		processStatuses = append(processStatuses, api.ProcessStatus{
			Name:         status.Name,
			State:        string(status.State),
			Desired:      string(status.Desired),
			Health:       string(status.Health),
			Reason:       status.Reason,
			PID:          status.PID,
			ExitCode:     status.ExitCode,
			RestartCount: status.RestartCount,
			Uptime:       status.Uptime,
		})
	}

	return &api.Response{
		Success: true,
		Data:    map[string]any{"processes": processStatuses},
	}
}

// selectProcess narrows a status list to one program, or to nothing if that
// program is not configured
func selectProcess(statuses []ProcessStatusInfo, name string) []ProcessStatusInfo {
	for _, status := range statuses {
		if status.Name == name {
			return []ProcessStatusInfo{status}
		}
	}
	return nil
}

// handleStart starts a process
func (s *IPCServer) handleStart(name string) *api.Response {
	if err := s.server.StartProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s started", name)}
}

// handleStop stops a process
func (s *IPCServer) handleStop(name string) *api.Response {
	if err := s.server.StopProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s stopped", name)}
}

// handleRestart restarts a process
func (s *IPCServer) handleRestart(name string) *api.Response {
	if err := s.server.RestartProcess(name); err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	return &api.Response{Success: true, Message: fmt.Sprintf("process %s restarted", name)}
}

// handleReload reloads the configuration and reports what it applied
func (s *IPCServer) handleReload() *api.Response {
	applied, err := s.server.Reload()
	if err != nil {
		return &api.Response{Success: false, Message: err.Error()}
	}
	if applied.Empty() {
		return &api.Response{Success: true, Message: "configuration reloaded, nothing changed"}
	}
	return &api.Response{Success: true, Message: "configuration reloaded", Data: applied}
}

// handleShutdown shuts down the supavisor
func (s *IPCServer) handleShutdown() *api.Response {
	go func() {
		// Send SIGTERM to ourselves
		pid := os.Getpid()
		proc, err := os.FindProcess(pid)
		if err != nil {
			slog.Error("failed to find process", "pid", pid, "error", err)
			return
		}
		err = proc.Signal(syscall.SIGTERM)
		if err != nil {
			slog.Error("failed to send SIGTERM", "error", err)
		}
	}()
	return &api.Response{Success: true, Message: "shutdown initiated"}
}
