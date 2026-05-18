// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mergeenqueue centralizes pr:mergeability job enqueueing so
// triggers across the codebase (PR create, head sync, check completion,
// review submit) share a single, side-effect-only helper. Lives outside
// the pulls orchestrator package to break the
// pulls → actions/trigger → actions/checksync import cycle.
//
// Best-effort: every helper here logs on failure and returns nothing.
// pr:mergeability is idempotent — a missed enqueue gets picked up by
// the next trigger.
package mergeenqueue

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// ForPR enqueues a pr:mergeability job for prID. trigger is a short
// label used for logs / future metrics ("pr_create", "head_sync",
// "check_complete", "review_submit").
func ForPR(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, prID int64, trigger string) {
	if pool == nil || prID == 0 {
		return
	}
	if _, err := worker.Enqueue(ctx, pool, worker.KindPRMergeability,
		map[string]any{"pr_id": prID}, worker.EnqueueOptions{}); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "mergeenqueue: enqueue",
				"error", err, "pr_id", prID, "trigger", trigger)
		}
		return
	}
	_ = worker.Notify(ctx, pool)
}

// ForHeadSHA fans out ForPR to every open PR whose head_oid matches
// headSHA in the given repo. Use after a check run for that SHA
// transitions to "completed".
func ForHeadSHA(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, repoID int64, headSHA string) {
	if pool == nil || headSHA == "" {
		return
	}
	prIDs, err := pullsdb.New().ListOpenPRsForHeadSHA(ctx, pool, pullsdb.ListOpenPRsForHeadSHAParams{
		HeadRepoID: repoID,
		HeadOid:    headSHA,
	})
	if err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "mergeenqueue: list open PRs",
				"error", err, "repo_id", repoID, "head_sha", headSHA)
		}
		return
	}
	for _, prID := range prIDs {
		ForPR(ctx, pool, logger, prID, "check_complete")
	}
}
