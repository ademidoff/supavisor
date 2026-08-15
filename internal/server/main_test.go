package server

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/ademidoff/supavisor/internal/process"
)

// TestMain starts the reaper for the whole binary.
//
// It is process-wide, and these tests start managed programs, so it would come
// up anyway on the first one to spawn. Starting it here instead makes that
// deterministic: helpers that observe a child's death by signal rather than by
// waiting depend on something collecting it, and which test ran first is not a
// sound thing to depend on.
func TestMain(m *testing.M) {
	process.StartReaping(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
