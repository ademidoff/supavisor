package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ademidoff/supavisor/internal/api"
)

// reloadResponse builds a reload response the way one arrives over the socket,
// by round-tripping the payload through JSON so the test sees the same generic
// shape the CLI has to decode
func reloadResponse(t *testing.T, applied api.ReloadResponse) api.Response {
	t.Helper()

	raw, err := json.Marshal(applied)
	if err != nil {
		t.Fatalf("Failed to marshal the diff: %v", err)
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("Failed to unmarshal the diff: %v", err)
	}
	return api.Response{Success: true, Message: "configuration reloaded", Data: data}
}

func TestPrintReload_NamesWhatItApplied(t *testing.T) {
	out := captureStdout(t, func() {
		printReload(reloadResponse(t, api.ReloadResponse{
			Added:   []string{"vmproxy"},
			Removed: []string{"nomad-server"},
			Changed: []string{"victoriametrics", "vmalert"},
		}))
	})

	for _, want := range []string{
		"added: vmproxy",
		"removed: nomad-server",
		"changed: victoriametrics, vmalert",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Expected %s in the output, got:\n%s", want, out)
		}
	}
}

// TestPrintReload_SkipsEmptyCategories covers the common case: one category has
// something in it and the other two should not be mentioned at all.
func TestPrintReload_SkipsEmptyCategories(t *testing.T) {
	out := captureStdout(t, func() {
		printReload(reloadResponse(t, api.ReloadResponse{Changed: []string{"grafana"}}))
	})

	if !strings.Contains(out, "changed: grafana") {
		t.Errorf("Expected the changed program to be named, got:\n%s", out)
	}
	if strings.Contains(out, "added") || strings.Contains(out, "removed") {
		t.Errorf("Empty categories should not be printed, got:\n%s", out)
	}
}

// TestPrintReload_NoOp covers a reload that changed nothing, which carries no
// data at all and should print only the daemon's message.
func TestPrintReload_NoOp(t *testing.T) {
	out := captureStdout(t, func() {
		printReload(api.Response{Success: true, Message: "configuration reloaded, nothing changed"})
	})

	if !strings.Contains(out, "nothing changed") {
		t.Errorf("Expected the daemon's message, got:\n%s", out)
	}
	if strings.Contains(out, ":") {
		t.Errorf("A no-op reload should not print any category, got:\n%s", out)
	}
}
