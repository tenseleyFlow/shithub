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
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsevent "github.com/tenseleyFlow/shithub/internal/actions/event"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/cache/pagecache"
	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
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
				// S28: kick a code-search reindex of the new
				// default-branch tip. The reindex job is idempotent
				// + atomic-swap, so a concurrent push that lands
				// before this finishes simply gets re-indexed on
				// the NEXT push. The reconciler heals any drift.
				if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoIndexCode,
					map[string]any{"repo_id": repo.ID},
					worker.EnqueueOptions{}); err != nil {
					deps.Logger.WarnContext(ctx, "push:process: enqueue index_code",
						"push_event_id", event.ID, "error", err)
				}
				// SP24: refresh repository insights from the same
				// default-branch tip. The job is intentionally
				// separate from code search so expensive history
				// aggregation never runs in the push processor.
				if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoInsightsRecalc,
					map[string]any{"repo_id": repo.ID},
					worker.EnqueueOptions{}); err != nil {
					deps.Logger.WarnContext(ctx, "push:process: enqueue insights_recalc",
						"push_event_id", event.ID, "error", err)
				}
				// SP25: refresh dependency inventory for the
				// default-branch tip. The worker only parses supported
				// manifests and matches local advisories; it never calls
				// external vulnerability services on the push path.
				if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoDependencyScan,
					map[string]any{"repo_id": repo.ID},
					worker.EnqueueOptions{}); err != nil {
					deps.Logger.WarnContext(ctx, "push:process: enqueue dependency_scan",
						"push_event_id", event.ID, "error", err)
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

		// 4d: actions trigger (S41b). Skip on branch deletes (after = zero
		// SHA — there's no commit to discover workflows at). For all
		// other pushes, enqueue a workflow:trigger job; the handler
		// discovers `.shithub/workflows/*.yml` at after-sha, matches
		// against the push event, and persists matching runs as queued.
		// Best-effort: a failure to enqueue the trigger shouldn't roll
		// back the whole push pipeline.
		if !isZeroSHA(event.AfterSha) && strings.HasPrefix(event.Ref, refPrefix) {
			if err := enqueueActionsTrigger(ctx, deps, repo, event); err != nil {
				deps.Logger.WarnContext(ctx, "push:process: enqueue workflow:trigger",
					"push_event_id", event.ID, "error", err)
			}
		}

		// 5: mark processed last so a partial failure earlier triggers a
		// retry that retries the whole pipeline. Idempotency is via the
		// processed_at guard at the top.
		if err := wq.MarkPushEventProcessed(ctx, deps.Pool, event.ID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		if actorID := int64ValueOrZero(event.PusherUserID); actorID != 0 {
			if err := social.Emit(ctx, social.Deps{Pool: deps.Pool, Logger: deps.Logger}, social.EmitParams{
				ActorUserID: actorID,
				Kind:        "push",
				RepoID:      event.RepoID,
				SourceKind:  "repo",
				SourceID:    event.RepoID,
				Public:      repo.Visibility == reposdb.RepoVisibilityPublic,
				Payload:     body,
			}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "push:process: emit push event",
					"push_event_id", event.ID, "error", err)
			}
		}

		// F01 PR-4: tell the web process to drop any cached
		// commits-page renders for the pre-push head OID. The cache
		// key is (repo_id, oid, page) so post-push requests already
		// miss the old entries naturally; this NOTIFY is the
		// memory-recovery + belt-and-suspenders layer. Best effort —
		// the 60s TTL on the cache is the safety net.
		if !isZeroSHA(event.BeforeSha) {
			if err := pagecache.Publish(ctx, deps.Pool, event.RepoID, event.BeforeSha); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "push:process: pagecache invalidate",
					"push_event_id", event.ID, "error", err)
			}
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

// enqueueActionsTrigger builds a workflow:trigger job from a processed
// push_event and writes it to the worker queue. The handler will
// discover workflows at the after-sha and persist matching runs.
//
// trigger_event_id = "push:<push_event_id>" — stable across retries
// of this push_process pipeline so the trigger handler's ON CONFLICT
// dedup catches duplicates cleanly. Re-runs of the same push (a rare
// admin action) would explicitly use a different trigger_event_id.
func enqueueActionsTrigger(ctx context.Context, deps PushProcessDeps, repo reposdb.Repo, event workerdb.PushEvent) error {
	const refPrefix = "refs/heads/"
	const tagPrefix = "refs/tags/"
	branch, tag := "", ""
	switch {
	case strings.HasPrefix(event.Ref, refPrefix):
		branch = event.Ref[len(refPrefix):]
	case strings.HasPrefix(event.Ref, tagPrefix):
		tag = event.Ref[len(tagPrefix):]
	}

	ownerLogin, err := resolvePushOwnerLogin(ctx, deps.Pool, repo)
	if err != nil {
		return fmt.Errorf("resolve owner: %w", err)
	}
	gitDir, err := deps.RepoFS.RepoPath(ownerLogin, repo.Name)
	if err != nil {
		return fmt.Errorf("repo path: %w", err)
	}
	changed, err := gitops.ChangedPaths(ctx, gitDir, event.BeforeSha, event.AfterSha)
	if err != nil {
		// Non-fatal: with no changed paths, paths-filtered workflows
		// won't trigger but everything else still will. Log and
		// continue so a transient git error doesn't block CI.
		deps.Logger.WarnContext(ctx, "push:process: changed-paths failed",
			"push_event_id", event.ID, "error", err)
		changed = nil
	}

	authorLogin := ""
	if event.PusherUserID.Valid {
		if u, err := usersdb.New().GetUserByID(ctx, deps.Pool, event.PusherUserID.Int64); err == nil {
			authorLogin = u.Username
		}
	}
	payload := actionsevent.Push(event.Ref, event.BeforeSha, event.AfterSha, actionsevent.HeadCommit{
		ID:     event.AfterSha,
		Author: authorLogin,
	})

	job := trigger.JobPayload{
		RepoID:         repo.ID,
		HeadSHA:        event.AfterSha,
		HeadRef:        event.Ref,
		EventKind:      trigger.EventPush,
		EventPayload:   payload,
		ActorUserID:    int64ValueOrZero(event.PusherUserID),
		TriggerEventID: fmt.Sprintf("push:%d", event.ID),
		Branch:         branch,
		Tag:            tag,
		ChangedPaths:   changed,
	}
	if _, err := worker.Enqueue(ctx, deps.Pool, trigger.KindWorkflowTrigger, job, worker.EnqueueOptions{}); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

// resolvePushOwnerLogin maps a repo's owner FK to the short login.
// Mirrors trigger.resolveOwnerLogin (we keep them per-package rather
// than centralizing a lookup helper since the call sites are sparse).
func resolvePushOwnerLogin(ctx context.Context, pool dbConn, repo reposdb.Repo) (string, error) {
	if repo.OwnerUserID.Valid {
		u, err := usersdb.New().GetUserByID(ctx, pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}
	if repo.OwnerOrgID.Valid {
		o, err := orgsdb.New().GetOrgByID(ctx, pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", err
		}
		return o.Slug, nil
	}
	return "", errors.New("repo has neither owner_user_id nor owner_org_id")
}

// dbConn is the minimal sqlc-DBTX surface our owner lookup needs.
type dbConn interface {
	usersdb.DBTX
}
