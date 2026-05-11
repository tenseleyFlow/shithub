// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "shithubd-runner",
	Short:         "Run shithub Actions jobs on a Docker or Podman host",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "shithubd-runner:", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(versionCmd)
}
