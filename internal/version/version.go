// Package version reports the build identity of a supavisor binary.
package version

import "fmt"

// Overridden at link time with -ldflags -X by the release pipeline. The
// defaults are what a plain `go build` from a working tree reports.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the one-line version banner for the named binary
func String(name string) string {
	return fmt.Sprintf("%s %s (%s, %s)", name, Version, Commit, Date)
}
