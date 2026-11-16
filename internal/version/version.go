// Package version holds build information, set at link time by GoReleaser:
//
//	-X github.com/teraldev/cli/internal/version.Version=1.2.3
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version, without a leading "v".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build date in RFC 3339.
	Date = "unknown"
)

// UserAgent identifies the CLI to the API, which records deployments it
// creates as triggered by the CLI.
func UserAgent() string {
	return fmt.Sprintf("teral-cli/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}

// Client is the value of the X-Teral-Client header.
func Client() string {
	return "teral-cli/" + Version
}
