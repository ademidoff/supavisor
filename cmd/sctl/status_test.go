package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ademidoff/supavisor/internal/api"
)

// statusResponse builds a status response the way one arrives over the socket,
// where every number has been through JSON and is a float64
func statusResponse(processes ...map[string]any) api.Response {
	rows := make([]any, 0, len(processes))
	for _, p := range processes {
		rows = append(rows, p)
	}
	return api.Response{Success: true, Data: map[string]any{"processes": rows}}
}

func blockedProcess() map[string]any {
	return map[string]any{
		"name": "api", "state": "STOPPED", "desired": "RUNNING", "health": "NONE",
		"pid": float64(0), "exit_code": float64(0), "restart_count": float64(0),
		"uptime": "N/A",
		"reason": "dependency db is running but its health check is UNHEALTHY",
	}
}

func runningProcess() map[string]any {
	return map[string]any{
		"name": "db", "state": "RUNNING", "desired": "RUNNING", "health": "HEALTHY",
		"pid": float64(4211), "exit_code": float64(0), "restart_count": float64(0),
		"uptime": "2m 10s",
	}
}

func captureStdout(t *testing.T, render func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}

	original := os.Stdout
	os.Stdout = writer
	render()
	os.Stdout = original
	_ = writer.Close()

	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read captured output: %v", err)
	}
	return string(out)
}

// TestPrintStatus_ShowsWhatIsWanted covers the column that tells a program
// nobody asked for apart from one that cannot start: both are STOPPED.
func TestPrintStatus_ShowsWhatIsWanted(t *testing.T) {
	out := captureStdout(t, func() {
		printStatus(statusResponse(blockedProcess(), runningProcess()))
	})

	if !strings.Contains(out, "DESIRED") {
		t.Errorf("Expected a DESIRED column, got:\n%s", out)
	}
	if !strings.Contains(out, "STOPPED   RUNNING") {
		t.Errorf("Expected the blocked program to be STOPPED but wanted, got:\n%s", out)
	}
	// The table stays narrow: the reason belongs to the detail view
	if strings.Contains(out, "health check is UNHEALTHY") {
		t.Errorf("The table should not carry the reason, got:\n%s", out)
	}
}

func TestPrintProcessDetail_ExplainsWhyAProgramIsWaiting(t *testing.T) {
	out := captureStdout(t, func() {
		printProcessDetail(statusResponse(blockedProcess()))
	})

	for _, want := range []string{
		"Name:", "api",
		"State:", "STOPPED",
		"Desired:", "RUNNING",
		"Reason:", "dependency db is running but its health check is UNHEALTHY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Expected the detail to contain %s, got:\n%s", want, out)
		}
	}
}

// TestPrintProcessDetail_SaysNothingWhenThereIsNothingToExplain keeps the line
// out of the way of a program that is simply running
func TestPrintProcessDetail_SaysNothingWhenThereIsNothingToExplain(t *testing.T) {
	out := captureStdout(t, func() {
		printProcessDetail(statusResponse(runningProcess()))
	})

	if strings.Contains(out, "Reason:") {
		t.Errorf("A running program has nothing to explain, got:\n%s", out)
	}
	if !strings.Contains(out, "HEALTHY") {
		t.Errorf("Expected the health to be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "4211") {
		t.Errorf("Expected the pid of a running program, got:\n%s", out)
	}
}

// TestPrintProcessDetail_ReportsTheReasonUnderOneLabel covers the other way a
// program can be wanted and not running: nothing is holding it back, it stopped
// trying. Both cases read under the same label, in the same place.
func TestPrintProcessDetail_ReportsTheReasonUnderOneLabel(t *testing.T) {
	fatal := map[string]any{
		"name": "doomed", "state": "FATAL", "desired": "RUNNING", "health": "NONE",
		"pid": float64(0), "exit_code": float64(1), "restart_count": float64(1),
		"uptime": "N/A", "reason": "gave up after 1 restart",
	}

	out := captureStdout(t, func() {
		printProcessDetail(statusResponse(fatal))
	})

	if !strings.Contains(out, "Reason:") || !strings.Contains(out, "gave up after 1 restart") {
		t.Errorf("Expected the detail to say it gave up, got:\n%s", out)
	}
}

func TestHealthColumn(t *testing.T) {
	tests := []struct {
		health string
		want   string
	}{
		{health: "", want: "-"},
		{health: "NONE", want: "-"},
		{health: "HEALTHY", want: "HEALTHY"},
		{health: "UNHEALTHY", want: "UNHEALTHY"},
	}

	for _, tt := range tests {
		if got := healthColumn(tt.health); got != tt.want {
			t.Errorf("healthColumn(%s) = %s, want %s", tt.health, got, tt.want)
		}
	}
}

// TestPidColumn keeps a stale pid from being reported for a program that is not
// running, where it names whatever now holds that number
func TestPidColumn(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{state: "RUNNING", want: "42"},
		{state: "STARTING", want: "42"},
		{state: "STOPPING", want: "42"},
		{state: "STOPPED", want: "N/A"},
		{state: "EXITED", want: "N/A"},
		{state: "FATAL", want: "N/A"},
	}

	for _, tt := range tests {
		if got := pidColumn(tt.state, 42); got != tt.want {
			t.Errorf("pidColumn(%s) = %s, want %s", tt.state, got, tt.want)
		}
	}
}
