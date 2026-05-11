// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version, commit, build time, and Go runtime",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("shithubd-runner %s\n", version.Version)
		fmt.Printf("  commit: %s\n", version.Commit)
		fmt.Printf("  built:  %s\n", version.BuiltAt)
		fmt.Printf("  go:     %s\n", runtime.Version())
		fmt.Printf("  os:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
	},
}
