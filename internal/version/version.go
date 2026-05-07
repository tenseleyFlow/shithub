// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version exposes build-time information embedded via -ldflags.
//
// Values are set by the Makefile at build time:
//
//	-X github.com/tenseleyFlow/shithub/internal/version.Version=...
//	-X github.com/tenseleyFlow/shithub/internal/version.Commit=...
//	-X github.com/tenseleyFlow/shithub/internal/version.BuiltAt=...
package version

// Build-time injected values. Defaults reflect a non-release `go run` build.
var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

// String returns a one-line stamp suitable for `--version` style output.
func String() string {
	return Version + " (" + Commit + ", built " + BuiltAt + ")"
}
