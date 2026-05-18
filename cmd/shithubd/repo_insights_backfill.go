// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// repoInsightsBackfillAllCmd is the operator-facing reconciliation pass for
// repositories that existed before SP24's snapshot worker shipped.
var repoInsightsBackfillAllCmd = &cobra.Command{
	Use:   "repo-insights-backfill-all",
	Short: "Enqueue repository insights snapshots for every active repo",
	Long: `Walks every active repo and enqueues a repo:insights_recalc worker
job for each one. The worker recomputes Pulse, Contributors, Commits, and Code
frequency snapshots from the repository's default branch.

This command does not compute git history inline. It only enqueues jobs, so it
is safe to run during deploys and after repairing worker drift.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("repo-insights-backfill-all: DB not configured")
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			return fmt.Errorf("db open: %w", err)
		}
		defer pool.Close()

		repos, err := reposdb.New().ListAllActiveReposWithOwner(ctx, pool)
		if err != nil {
			return fmt.Errorf("list repos: %w", err)
		}
		var count int
		for _, repo := range repos {
			if _, err := worker.Enqueue(ctx, pool, worker.KindRepoInsightsRecalc,
				map[string]any{"repo_id": repo.ID}, worker.EnqueueOptions{}); err != nil {
				return fmt.Errorf("enqueue repo %d: %w", repo.ID, err)
			}
			count++
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"repo-insights-backfill-all: enqueued %d job(s); follow progress via worker logs\n",
			count,
		)
		return nil
	},
}
