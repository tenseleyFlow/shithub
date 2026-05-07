// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect the resolved shithub configuration",
}

var configPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the active configuration with secrets redacted",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		out, err := config.PrintRedacted(cfg)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate config (exits non-zero on failure)",
	RunE: func(_ *cobra.Command, _ []string) error {
		_, err := config.Load(nil)
		return err
	},
}

func init() {
	configCmd.AddCommand(configPrintCmd)
	configCmd.AddCommand(configValidateCmd)
}
