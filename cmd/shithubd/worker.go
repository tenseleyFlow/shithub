// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/worker"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

// auditRecorder returns the shared audit recorder. Kept as a function
// rather than a package-level value so future tests / non-default
// recorders can substitute via dependency injection.
func auditRecorder() *audit.Recorder { return audit.NewRecorder() }

// workerCmd boots a long-running worker pool. SIGINT/SIGTERM trigger
// graceful shutdown: the LISTEN goroutine drops, claim attempts stop,
// in-flight jobs are given a deadline to finish, then the binary exits.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run background workers (push processing, size recalc, purge)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		workersFlag, _ := cmd.Flags().GetInt("workers")

		cfg, err := config.Load(nil)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if cfg.DB.URL == "" {
			return errors.New("worker: SHITHUB_DATABASE_URL unset")
		}
		root, err := filepath.Abs(cfg.Storage.ReposRoot)
		if err != nil {
			return fmt.Errorf("repos_root: %w", err)
		}
		rfs, err := storage.NewRepoFS(root)
		if err != nil {
			return fmt.Errorf("repo fs: %w", err)
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Worker count: flag overrides env override default.
		count := workersFlag
		if count <= 0 {
			if v, _ := strconv.Atoi(os.Getenv("SHITHUB_WORKERS")); v > 0 {
				count = v
			}
		}

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: int32(count) + 2, MinConns: 1,
		})
		if err != nil {
			return fmt.Errorf("db: %w", err)
		}
		defer pool.Close()

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

		p := worker.NewPool(pool, worker.PoolConfig{
			Workers:    count,
			IdlePoll:   5 * time.Second,
			JobTimeout: 5 * time.Minute,
			Logger:     logger,
		})
		p.Register(worker.KindPushProcess, jobs.PushProcess(jobs.PushProcessDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindRepoSizeRecalc, jobs.RepoSizeRecalc(jobs.RepoSizeRecalcDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindJobsPurge, jobs.JobsPurge(jobs.JobsPurgeDeps{
			Pool: pool, Logger: logger,
		}))
		p.Register(worker.KindLifecycleSweep, jobs.LifecycleSweep(jobs.LifecycleSweepDeps{
			Pool: pool, RepoFS: rfs, Audit: auditRecorder(), Logger: logger,
		}))
		prDeps := jobs.PRJobsDeps{Pool: pool, RepoFS: rfs, Logger: logger}
		p.Register(worker.KindPRSynchronize, jobs.PRSynchronize(prDeps))
		p.Register(worker.KindPRMergeability, jobs.PRMergeability(prDeps))
		p.Register(worker.KindPRMerge, jobs.PRMerge(prDeps))

		return p.Run(ctx)
	},
}

func init() {
	workerCmd.Flags().Int("workers", 0, "Number of worker goroutines (default 4)")
	rootCmd.AddCommand(workerCmd)
}
