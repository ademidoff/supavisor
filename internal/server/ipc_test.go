package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ademidoff/supavisor/internal/api"
	"github.com/ademidoff/supavisor/internal/config"
)

// dialTestIPC connects to a test IPC socket
func dialTestIPC(t *testing.T, path string) net.Conn {
	t.Helper()

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	return conn
}

func startTestIPC(t *testing.T) *IPCServer {
	t.Helper()

	tmpDir, err := os.MkdirTemp("/tmp", "sv-ipc")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	srv := &Server{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		processes: nil,
	}
	ipc := NewIPCServer(filepath.Join(tmpDir, "s.sock"), "", srv)
	if err := ipc.Start(); err != nil {
		t.Fatalf("IPC Start failed: %v", err)
	}
	t.Cleanup(func() { _ = ipc.Stop() })
	return ipc
}

// TestSocketIsNotWorldWritable is the regression test for a control socket any
// local user could drive. Writing to it stops supervised processes, and start
// runs configured commands as the daemon's user.
func TestSocketIsNotWorldWritable(t *testing.T) {
	ipc := startTestIPC(t)

	info, err := os.Stat(ipc.socketPath)
	if err != nil {
		t.Fatalf("Failed to stat socket: %v", err)
	}

	mode := info.Mode().Perm()
	if mode&0o007 != 0 {
		t.Errorf("Socket mode is %04o, which grants access to other users", mode)
	}
	if mode != socketMode {
		t.Errorf("Socket mode is %04o, expected %04o", mode, socketMode)
	}
}

func TestIPC_RejectsAnOversizedRequest(t *testing.T) {
	ipc := startTestIPC(t)

	conn := dialTestIPC(t, ipc.socketPath)
	defer conn.Close()

	// A body past the limit must be cut off rather than buffered
	oversized := `{"command":"status","args":["` + strings.Repeat("x", maxRequestBytes*2) + `"]}`
	_, _ = conn.Write([]byte(oversized))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp api.Response
	if err := json.NewDecoder(conn).Decode(&resp); err == nil {
		t.Error("Expected the oversized request to be refused, got a response")
	}
}

func TestIPC_ServesAfterAClientIsDropped(t *testing.T) {
	ipc := startTestIPC(t)

	// A client that connects and vanishes must not take the daemon with it
	for range 5 {
		_ = dialTestIPC(t, ipc.socketPath).Close()
	}

	conn := dialTestIPC(t, ipc.socketPath)
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(&api.Request{Command: "status"}); err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var resp api.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}
	if !resp.Success {
		t.Errorf("Expected a successful status response, got: %s", resp.Message)
	}
}

func TestIPC_BoundsConcurrentConnections(t *testing.T) {
	ipc := startTestIPC(t)

	// Idle clients beyond the cap queue rather than being served, and none of
	// them may take the daemon down.
	conns := make([]net.Conn, 0, maxConnections+4)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})

	for range maxConnections + 4 {
		conns = append(conns, dialTestIPC(t, ipc.socketPath))
	}

	if got := len(ipc.slots); got > maxConnections {
		t.Errorf("Serving %d connections, past the cap of %d", got, maxConnections)
	}
}

// TestIPC_StatusForOneProcess covers the detail view sctl asks for when it is
// given a program name
func TestIPC_StatusForOneProcess(t *testing.T) {
	sv := newTestServer(t, map[string]*config.ProgramConfig{
		"api": {Command: "/bin/sleep 60", StartSecs: 1, MaxRestarts: 1},
		"db":  {Command: "/bin/sleep 60", StartSecs: 1, MaxRestarts: 1},
	})
	ipc := &IPCServer{server: sv}

	all := ipc.handleStatus(nil)
	if got := len(processesOf(t, all)); got != 2 {
		t.Errorf("Expected every program without a name, got %d", got)
	}

	one := processesOf(t, ipc.handleStatus([]string{"api"}))
	if len(one) != 1 || one[0].Name != "api" {
		t.Fatalf("Expected only api, got %v", one)
	}

	missing := ipc.handleStatus([]string{"nope"})
	if missing.Success {
		t.Fatal("Expected an unknown program to be reported as an error")
	}
	if !strings.Contains(missing.Message, "nope") {
		t.Errorf("Expected the error to name the program, got: %s", missing.Message)
	}
}

func processesOf(t *testing.T, resp *api.Response) []api.ProcessStatus {
	t.Helper()

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected status data, got %#v", resp.Data)
	}
	statuses, ok := data["processes"].([]api.ProcessStatus)
	if !ok {
		t.Fatalf("Expected a process list, got %#v", data["processes"])
	}
	return statuses
}

func TestLookupGroupID_AcceptsANumericGroup(t *testing.T) {
	gid, err := lookupGroupID("20")
	if err != nil {
		t.Fatalf("lookupGroupID failed: %v", err)
	}
	if gid != 20 {
		t.Errorf("Expected gid 20, got %d", gid)
	}
}

func TestLookupGroupID_RejectsAnUnknownGroup(t *testing.T) {
	if _, err := lookupGroupID("definitely-not-a-real-group"); err == nil {
		t.Error("Expected an error for an unknown group")
	}
}

// TestIPC_StopWhileConnectionsArrive is the regression test for a WaitGroup
// misuse between the accept loop and Stop.
//
// The loop adds to the connection group for every client it accepts, while Stop
// waits on that group. An Add that begins once the counter is zero and a Wait is
// already running is not allowed, and the race detector reports it as a data
// race. Closing the listener does not settle it on its own: an accept that has
// already returned is still on its way to Add.
//
// The window is small, so this drives many stop cycles rather than one. Without
// the fix it reports a race within a couple of runs; with it, not in hundreds.
func TestIPC_StopWhileConnectionsArrive(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "sv-ipc-stop")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ctx := t.Context()

	for cycle := range 40 {
		socket := filepath.Join(tmpDir, fmt.Sprintf("s%d.sock", cycle))
		srv := &Server{
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			processes: nil,
		}
		ipc := NewIPCServer(socket, "", srv)
		if err := ipc.Start(); err != nil {
			t.Fatalf("IPC Start failed: %v", err)
		}

		// Keep clients arriving, so an accept is in flight when Stop runs.
		stopDialing := make(chan struct{})
		var dialers sync.WaitGroup
		for range 4 {
			dialers.Go(func() {
				var dialer net.Dialer
				for {
					select {
					case <-stopDialing:
						return
					default:
					}
					conn, dialErr := dialer.DialContext(ctx, "unix", socket)
					if dialErr == nil {
						_ = conn.Close()
					}
				}
			})
		}

		// Long enough for the first clients to be served and the counter to
		// fall back to zero, which is the state an Add must not race.
		time.Sleep(5 * time.Millisecond)

		if err := ipc.Stop(); err != nil {
			t.Errorf("Stop failed on cycle %d: %v", cycle, err)
		}

		close(stopDialing)
		dialers.Wait()
	}
}
