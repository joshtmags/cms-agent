// Package version holds the agent's version, set at build time with
// -ldflags "-X github.com/joshtmags/cms-agent/internal/version.Version=1.2.3".
package version

// Version is "0.0.0" for local builds, which every real release outranks.
var Version = "0.0.0"
