package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ademidoff/supavisor/internal/api"
)

// decodeJSONOutput parses what printJSON wrote, which is the point of the
// format: a consumer has to be able to parse it without knowing the layout
func decodeJSONOutput(t *testing.T, out string) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("Output is not valid JSON: %v\n%s", err, out)
	}
	return decoded
}

func TestPrintJSON_CarriesTheStatusPayload(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printJSON(statusResponse(runningProcess())); err != nil {
			t.Fatalf("printJSON failed: %v", err)
		}
	})

	decoded := decodeJSONOutput(t, out)
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatalf("Expected a data object, got:\n%s", out)
	}
	processes, ok := data["processes"].([]any)
	if !ok || len(processes) != 1 {
		t.Fatalf("Expected one process, got:\n%s", out)
	}

	row, ok := processes[0].(map[string]any)
	if !ok {
		t.Fatalf("Expected a process object, got:\n%s", out)
	}
	// The fields a consumer reads instead of scraping columns, including the
	// two that make the table ambiguous when grepped.
	for _, field := range []string{"name", "state", "desired", "health"} {
		if _, present := row[field]; !present {
			t.Errorf("Expected %s in the payload, got:\n%s", field, out)
		}
	}
}

// TestPrintJSON_ReportsFailures covers the reason JSON is printed even when the
// command failed: a consumer should not have to scrape stderr for the error.
func TestPrintJSON_ReportsFailures(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printJSON(api.Response{Success: false, Message: "process nosuchprog not found"}); err != nil {
			t.Fatalf("printJSON failed: %v", err)
		}
	})

	decoded := decodeJSONOutput(t, out)
	if success, _ := decoded["success"].(bool); success {
		t.Errorf("Expected success to be false, got:\n%s", out)
	}
	if msg, _ := decoded["message"].(string); !strings.Contains(msg, "nosuchprog") {
		t.Errorf("Expected the failure message, got:\n%s", out)
	}
}

// TestPrintJSON_CarriesTheReloadDiff checks that the format is the same shape
// for every command, so the reload diff is machine-readable too.
func TestPrintJSON_CarriesTheReloadDiff(t *testing.T) {
	out := captureStdout(t, func() {
		resp := reloadResponse(t, api.ReloadResponse{Changed: []string{"victoriametrics"}})
		if err := printJSON(resp); err != nil {
			t.Fatalf("printJSON failed: %v", err)
		}
	})

	decoded := decodeJSONOutput(t, out)
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatalf("Expected a data object, got:\n%s", out)
	}
	changed, ok := data["changed"].([]any)
	if !ok || len(changed) != 1 || changed[0] != "victoriametrics" {
		t.Errorf("Expected the changed list, got:\n%s", out)
	}
}
