// Package version holds the Agent release version.
package version

// Version is the Agent release version. Release builds set it with
// -ldflags "-X github.com/iwaneo/hostbeacon/agent/internal/version.Version=<version>".
var Version = "0.0.0-dev"

// String returns the program name and version, for example "hostbeacon 0.0.0-dev".
func String() string {
	return "hostbeacon " + Version
}
