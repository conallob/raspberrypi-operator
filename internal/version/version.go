// Package version holds the build's identity. The values are set at link time:
//
//	-ldflags "-X github.com/conallob/raspberrypi-operator/internal/version.Version=1.2.3 ..."
//
// by GoReleaser (.goreleaser.yaml), the Makefile and the Dockerfile. A plain
// `go build` reports "dev".
package version

import "fmt"

// Set by -ldflags -X; the defaults identify an unreleased local build.
var (
	// Version is the release version without a leading "v" (e.g. 1.2.3), or
	// "dev".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build or commit time, RFC 3339.
	Date = "unknown"
)

// String is the one-line description logged at startup and printed by -version.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
