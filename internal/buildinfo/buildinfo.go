package buildinfo

import "fmt"

// These values are replaced at build time through -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a compact, human-readable build description.
func String() string {
	return fmt.Sprintf("promptd %s (commit %s, built %s)", Version, Commit, Date)
}
