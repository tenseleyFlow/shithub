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
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
)

// gpgBackfillAllCmd is the operator-facing entrypoint that fans out
// one gpg:backfill worker job per active repo. Typical usage: run
// once at S51 first-deploy so existing signed commits and tags get
// commit_verification_cache rows stamped, after which "Verified"
// badges appear retroactively across the site.
//
// Idempotent: re-running enqueues another wave of jobs. The workers
// themselves are idempotent thanks to UpsertCommitVerification's ON
// CONFLICT clause; the only cost of re-running is the redundant
// enqueue + per-commit verify work, not data integrity.
//
// This is NOT an inline-blocking command — it enqueues and returns.
// Watch worker logs (or `SELECT * FROM jobs WHERE kind = 'gpg:backfill'
// ORDER BY created_at DESC`) to follow progress.
var gpgBackfillAllCmd = &cobra.Command{
	Use:   "gpg-backfill-all",
	Short: "Enqueue GPG signature backfill for every active repo",
	Long: `Walks every active repo and enqueues a gpg:backfill worker job per
repo. Each job verifies every signed commit on the repo's default
branch + every annotated tag, writing commit_verification_cache
rows so the Verified badge appears retroactively.

This is the one-shot operator command. The per-user-key eager
backfill (DispatchForKey) is wired into the GPG-key add handler
and runs automatically on key upload — you only need to run this
on first deploy or after a known mass-key-upload event.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("gpg-backfill-all: DB not configured")
		}

		// 5-minute cap on the enqueue step. The enqueue work itself is
		// fast (one INSERT per repo + one NOTIFY); the cap is a
		// pessimistic ceiling for sites with millions of repos.
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

		count, err := sigverify.DispatchAll(ctx, pool)
		if err != nil {
			return fmt.Errorf("dispatch: %w", err)
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"gpg-backfill-all: enqueued %d job(s); follow progress via worker logs\n",
			count,
		)
		return nil
	},
}
