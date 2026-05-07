// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version, commit, build time, Go runtime, and configured observability sinks",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("shithubd %s\n", version.Version)
		fmt.Printf("  commit: %s\n", version.Commit)
		fmt.Printf("  built:  %s\n", version.BuiltAt)
		fmt.Printf("  go:     %s\n", runtime.Version())
		fmt.Printf("  os:     %s/%s\n", runtime.GOOS, runtime.GOARCH)

		cfg, err := config.Load(nil)
		if err != nil {
			fmt.Printf("  config: <invalid: %s>\n", err)
			return
		}
		fmt.Printf("  env:     %s\n", cfg.Env)
		fmt.Printf("  log:     %s/%s\n", cfg.Log.Format, cfg.Log.Level)
		fmt.Printf("  metrics: %s\n", boolStr(cfg.Metrics.Enabled))
		fmt.Printf("  tracing: %s\n", tracingStr(cfg.Tracing))
		fmt.Printf("  errrep:  %s\n", errrepStr(cfg.ErrorReporting))
	},
}

func boolStr(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func tracingStr(t config.TracingConfig) string {
	if !t.Enabled {
		return "disabled"
	}
	return fmt.Sprintf("enabled (endpoint=%s, sample=%.2f)", t.Endpoint, t.SampleRate)
}

func errrepStr(e config.ErrorReportingConfig) string {
	if e.DSN == "" {
		return "disabled"
	}
	return "enabled (dsn=*** env=" + e.Environment + ")"
}
