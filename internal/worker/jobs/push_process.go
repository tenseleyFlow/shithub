// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jobs holds the concrete worker handlers wired into the pool
// at boot. Each file is one kind; handlers stay short and idempotent.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// PushProcessDeps wires the data this handler needs.
type PushProcessDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// PushProcessPayload is the JSON shape post-receive enqueues.
type PushProcessPayload struct {
	PushEventID int64 `json:"push_event_id"`
}

// PushProcess returns a handler that:
//
//  1. Loads the push_event by id.
//  2. Updates repos.default_branch_oid if the ref matches the default
//     branch and the after_sha is non-zero.
//  3. Enqueues a repo:size_recalc job (separate handler — du is
//     potentially slow, isolate it).
//  4. Inserts a webhook_events_pending row carrying the push payload
//     (S33 deliverer drains).
//  5. Marks the push_event processed.
//
// The handler is idempotent on processed_at: re-runs after the first
// successful run are no-ops.
func PushProcess(deps PushProcessDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p PushProcessPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.PushEventID == 0 {
			return worker.PoisonError(errors.New("missing push_event_id"))
		}

		wq := workerdb.New()
		event, err := wq.GetPushEvent(ctx, deps.Pool, p.PushEventID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("push_event %d not found", p.PushEventID))
			}
			return fmt.Errorf("load push_event: %w", err)
		}
		if event.ProcessedAt.Valid {
			return nil // idempotent: already done.
		}

		rq := reposdb.New()
		repo, err := rq.GetRepoByID(ctx, deps.Pool, event.RepoID)
		if err != nil {
			return fmt.Errorf("load repo: %w", err)
		}

		// 2: derive default-branch OID. The ref looks like "refs/heads/<name>".
		const refPrefix = "refs/heads/"
		if strings.HasPrefix(event.Ref, refPrefix) {
			branch := event.Ref[len(refPrefix):]
			if branch == repo.DefaultBranch {
				newOID := event.AfterSha
				if isZeroSHA(newOID) {
					// branch deleted — clear oid.
					_ = rq.UpdateRepoDefaultBranchOID(ctx, deps.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
						ID:               repo.ID,
						DefaultBranchOid: pgtype.Text{Valid: false},
					})
				} else {
					_ = rq.UpdateRepoDefaultBranchOID(ctx, deps.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
						ID:               repo.ID,
						DefaultBranchOid: pgtype.Text{String: newOID, Valid: true},
					})
				}
			}
		}

		// 3: enqueue size recalc — separate kind, runs independently.
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoSizeRecalc,
			map[string]any{"repo_id": repo.ID},
			worker.EnqueueOptions{}); err != nil {
			deps.Logger.WarnContext(ctx, "push:process: enqueue size_recalc",
				"push_event_id", event.ID, "error", err)
		}

		// 4: stash the payload for S33 to drain.
		body, _ := json.Marshal(map[string]any{
			"push_event_id":  event.ID,
			"repo_id":        event.RepoID,
			"pusher_user_id": int64ValueOrZero(event.PusherUserID),
			"before_sha":     event.BeforeSha,
			"after_sha":      event.AfterSha,
			"ref":            event.Ref,
			"protocol":       event.Protocol,
			"request_id":     event.RequestID,
		})
		if _, err := wq.InsertWebhookEventPending(ctx, deps.Pool, workerdb.InsertWebhookEventPendingParams{
			RepoID:    event.RepoID,
			EventKind: "push",
			Payload:   body,
		}); err != nil {
			return fmt.Errorf("insert webhook pending: %w", err)
		}

		// 4b: PR auto-synchronize. For any open PR whose head ref
		// matches the pushed ref, fan out a pr:synchronize job.
		// Best-effort — sync failures don't block the push pipeline.
		if strings.HasPrefix(event.Ref, refPrefix) {
			pq := pullsdb.New()
			prIDs, err := pq.ListOpenPRsForHeadRef(ctx, deps.Pool, pullsdb.ListOpenPRsForHeadRefParams{
				HeadRepoID: event.RepoID,
				HeadRef:    event.Ref[len(refPrefix):],
			})
			if err != nil {
				deps.Logger.WarnContext(ctx, "push:process: list PRs for sync",
					"push_event_id", event.ID, "error", err)
			}
			for _, prID := range prIDs {
				if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindPRSynchronize,
					map[string]any{"pr_id": prID}, worker.EnqueueOptions{}); err != nil {
					deps.Logger.WarnContext(ctx, "push:process: enqueue pr:synchronize",
						"pr_id", prID, "push_event_id", event.ID, "error", err)
				}
			}
		}

		// 5: mark processed last so a partial failure earlier triggers a
		// retry that retries the whole pipeline. Idempotency is via the
		// processed_at guard at the top.
		if err := wq.MarkPushEventProcessed(ctx, deps.Pool, event.ID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}

		// Wake any size_recalc workers waiting on LISTEN.
		_ = worker.Notify(ctx, deps.Pool)
		return nil
	}
}

func isZeroSHA(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return s != ""
}

func int64ValueOrZero(p pgtype.Int8) int64 {
	if p.Valid {
		return p.Int64
	}
	return 0
}
