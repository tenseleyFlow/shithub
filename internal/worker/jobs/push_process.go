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

	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"

	"path/filepath"
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

		// 4c: stale-on-push for required checks (S24). When a head ref
		// moves and the matching protection rule has
		// `dismiss_stale_status_checks_on_push = true`, mark every
		// not-yet-completed check suite on the previous SHA as
		// (completed, conclusion='stale'). Best-effort.
		if strings.HasPrefix(event.Ref, refPrefix) && !isZeroSHA(event.BeforeSha) {
			branch := event.Ref[len(refPrefix):]
			rules, err := rq.ListBranchProtectionRules(ctx, deps.Pool, repo.ID)
			if err == nil {
				rule, hasRule := longestMatchingRule(rules, branch)
				if hasRule && rule.DismissStaleStatusChecksOnPush {
					n, err := checks.MarkStaleForPreviousHead(ctx,
						checks.Deps{Pool: deps.Pool, Logger: deps.Logger},
						repo.ID, event.BeforeSha)
					if err != nil {
						deps.Logger.WarnContext(ctx, "push:process: mark stale checks",
							"push_event_id", event.ID, "error", err)
					} else if n > 0 {
						deps.Logger.InfoContext(ctx, "push:process: marked check suites stale",
							"push_event_id", event.ID, "count", n)
					}
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

// longestMatchingRule duplicates the longest-pattern-wins matcher
// from internal/repos/protection so this file doesn't take a circular
// import. Same semantics: alphabetical tiebreaker, no rule = not found.
func longestMatchingRule(rules []reposdb.BranchProtectionRule, branch string) (reposdb.BranchProtectionRule, bool) {
	var best reposdb.BranchProtectionRule
	bestLen := -1
	for _, r := range rules {
		ok, _ := filepath.Match(r.Pattern, branch)
		if !ok {
			continue
		}
		if len(r.Pattern) > bestLen ||
			(len(r.Pattern) == bestLen && r.Pattern < best.Pattern) {
			best = r
			bestLen = len(r.Pattern)
		}
	}
	return best, bestLen >= 0
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
