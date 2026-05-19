// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "shithubd",
	Short:         "shithub server: web, ssh, worker, and admin tooling",
	Long:          "shithubd is the entrypoint binary for shithub — a feature-complete, AGPL-licensed clone of GitHub.\nSubcommands cover the HTTP web server, the SSH dispatcher, background workers, migrations, and operational helpers.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "shithubd:", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(webCmd)
	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(seedCmd)

	// worker, hook, hooks (reinstall) registered in their own files.
	// ssh-authkeys and ssh-shell are registered in cmd/shithubd/ssh.go.
	rootCmd.AddCommand(storageCmd)
	rootCmd.AddCommand(adminCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(gpgBackfillAllCmd)
	rootCmd.AddCommand(repoInsightsBackfillAllCmd)
	rootCmd.AddCommand(repoDependencyScanBackfillAllCmd)
}
