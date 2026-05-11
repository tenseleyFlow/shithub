// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	infralog "github.com/tenseleyFlow/shithub/internal/infra/log"
	runnerpkg "github.com/tenseleyFlow/shithub/internal/runner"
	runnerapi "github.com/tenseleyFlow/shithub/internal/runner/api"
	runnerconfig "github.com/tenseleyFlow/shithub/internal/runner/config"
	"github.com/tenseleyFlow/shithub/internal/runner/engine"
	"github.com/tenseleyFlow/shithub/internal/runner/workspace"
)

var runConfigPath string

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Claim and execute shithub Actions jobs",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := runnerconfig.Load(runnerconfig.LoadOptions{
			ConfigPath: runConfigPath,
			Overrides:  flagOverrides(cmd),
		})
		if err != nil {
			return err
		}
		logger := infralog.New(infralog.Options{
			Level:  cfg.Log.Level,
			Format: cfg.Log.Format,
			Writer: os.Stderr,
		}).With("component", "runner")

		workspaces := workspace.New(cfg.Runner.WorkspaceRoot)
		removed, err := workspaces.Sweep(cfg.Runner.WorkspaceTTL, time.Now().UTC())
		if err != nil {
			return err
		}
		if removed > 0 {
			logger.InfoContext(cmd.Context(), "swept stale workspaces", "count", removed)
		}

		client, err := runnerapi.New(runnerapi.Config{
			BaseURL:     cfg.Server.BaseURL,
			RunnerToken: cfg.Runner.Token,
		})
		if err != nil {
			return err
		}
		execEngine := engine.NewDocker(engine.DockerConfig{
			Binary:       cfg.Engine.Kind,
			DefaultImage: cfg.Engine.DefaultImage,
			Network:      cfg.Engine.Network,
			Memory:       cfg.Engine.Memory,
			CPUs:         cfg.Engine.CPUs,
			Stdout:       os.Stdout,
			Stderr:       os.Stderr,
		})
		r := runnerpkg.New(runnerpkg.Options{
			API:          client,
			Engine:       execEngine,
			Workspaces:   workspaces,
			Logger:       logger,
			Labels:       cfg.Runner.Labels,
			Capacity:     cfg.Runner.Capacity,
			PollInterval: cfg.Runner.PollInterval,
			DefaultImage: cfg.Engine.DefaultImage,
			Clock:        func() time.Time { return time.Now().UTC() },
		})
		return r.Run(cmd.Context())
	},
}

func init() {
	runCmd.Flags().StringVar(&runConfigPath, "config", "", "Path to runner config file")
	runCmd.Flags().String("server-url", "", "shithub base URL")
	runCmd.Flags().String("token", "", "Runner registration token")
	runCmd.Flags().String("labels", "", "Comma-separated runner labels")
	runCmd.Flags().Int("capacity", 0, "Maximum concurrent jobs this runner advertises")
	runCmd.Flags().Duration("poll-interval", 0, "Idle heartbeat interval")
	runCmd.Flags().String("workspace-root", "", "Workspace root directory")
	runCmd.Flags().Duration("workspace-ttl", 0, "Startup sweep TTL for stale workspaces")
	runCmd.Flags().String("engine", "", "Execution engine: docker or podman")
	runCmd.Flags().String("image", "", "Default container image")
	runCmd.Flags().String("network", "", "Container network")
	runCmd.Flags().String("memory", "", "Container memory limit")
	runCmd.Flags().String("cpus", "", "Container CPU limit")
	runCmd.Flags().String("log-level", "", "Log level: debug, info, warn, error")
	runCmd.Flags().String("log-format", "", "Log format: text or json")
}

func flagOverrides(cmd *cobra.Command) map[string]string {
	keys := map[string]string{
		"server-url":     "server.base_url",
		"token":          "runner.token",
		"labels":         "runner.labels",
		"capacity":       "runner.capacity",
		"poll-interval":  "runner.poll_interval",
		"workspace-root": "runner.workspace_root",
		"workspace-ttl":  "runner.workspace_ttl",
		"engine":         "engine.kind",
		"image":          "engine.default_image",
		"network":        "engine.network",
		"memory":         "engine.memory",
		"cpus":           "engine.cpus",
		"log-level":      "log.level",
		"log-format":     "log.format",
	}
	out := make(map[string]string)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if key, ok := keys[f.Name]; ok {
			out[key] = f.Value.String()
		}
	})
	return out
}
