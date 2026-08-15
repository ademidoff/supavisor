package version

import "testing"

func TestStringUsesInjectedValues(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = origVersion, origCommit, origDate
	})

	Version, Commit, Date = "v1.2.3", "abc1234", "2026-01-02T03:04:05Z"

	got := String("supavisor")
	want := "supavisor v1.2.3 (abc1234, 2026-01-02T03:04:05Z)"
	if got != want {
		t.Errorf("String() = %s, want %s", got, want)
	}
}

func TestStringDefaults(t *testing.T) {
	// An unreleased build has no tag, commit or date injected, so it must still
	// print something rather than a banner full of empty fields.
	if Version != "dev" || Commit != "none" || Date != "unknown" {
		t.Fatalf("unexpected defaults: %s, %s, %s", Version, Commit, Date)
	}

	got := String("sctl")
	want := "sctl dev (none, unknown)"
	if got != want {
		t.Errorf("String() = %s, want %s", got, want)
	}
}
