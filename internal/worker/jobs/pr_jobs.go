// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsevent "github.com/tenseleyFlow/shithub/internal/actions/event"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// PRJobsDeps are shared by the three PR job handlers.
type PRJobsDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// PRSynchronizePayload — enqueued from push:process for any open PR
// whose head_repo_id+head_ref match the pushed ref.
type PRSynchronizePayload struct {
	PRID int64 `json:"pr_id"`
}

// PRSynchronize refreshes commits + files for a PR and queues a
// mergeability recompute.
func PRSynchronize(deps PRJobsDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p PRSynchronizePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.PRID == 0 {
			return worker.PoisonError(errors.New("missing pr_id"))
		}
		gitDir, err := resolveGitDirForPR(ctx, deps.Pool, deps.RepoFS, p.PRID)
		if err != nil {
			return err
		}
		if err := pulls.Synchronize(ctx, pulls.Deps{Pool: deps.Pool, Logger: deps.Logger}, gitDir, p.PRID); err != nil {
			return err
		}
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindPRDependencyReview,
			map[string]any{"pr_id": p.PRID}, worker.EnqueueOptions{}); err != nil {
			deps.Logger.WarnContext(ctx, "pr:synchronize: enqueue dependency review", "pr_id", p.PRID, "error", err)
		}
		// Chain a mergeability tick.
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindPRMergeability,
			map[string]any{"pr_id": p.PRID}, worker.EnqueueOptions{}); err != nil {
			deps.Logger.WarnContext(ctx, "pr:synchronize: enqueue mergeability", "pr_id", p.PRID, "error", err)
		}

		// Actions trigger (S41b). On PR head movement, fan out a
		// workflow:trigger with action="synchronize". Best-effort —
		// failures here log and let the rest of the synchronize chain
		// complete.
		if err := enqueuePRActionsTrigger(ctx, deps, p.PRID, "synchronize"); err != nil {
			deps.Logger.WarnContext(ctx, "pr:synchronize: enqueue workflow:trigger",
				"pr_id", p.PRID, "error", err)
		}

		_ = worker.Notify(ctx, deps.Pool)
		return nil
	}
}

// PRMergeabilityPayload — enqueued from synchronize and from PR
// open. Recomputes the merge-tree probe.
type PRMergeabilityPayload struct {
	PRID int64 `json:"pr_id"`
}

// PRMergeability runs `git merge-tree --write-tree` and persists the
// result.
func PRMergeability(deps PRJobsDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p PRMergeabilityPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.PRID == 0 {
			return worker.PoisonError(errors.New("missing pr_id"))
		}
		gitDir, err := resolveGitDirForPR(ctx, deps.Pool, deps.RepoFS, p.PRID)
		if err != nil {
			return err
		}
		return pulls.Mergeability(ctx, pulls.Deps{Pool: deps.Pool, Logger: deps.Logger}, gitDir, p.PRID)
	}
}

// resolveGitDirForPR turns a PR id into the bare-repo path on disk
// using the repo + owner-user lookups. Cached per-call only; the
// caller's job-level context bounds the work.
func resolveGitDirForPR(ctx context.Context, pool *pgxpool.Pool, rfs *storage.RepoFS, prID int64) (string, error) {
	pr, err := pullsdb.New().GetPullRequestByIssueID(ctx, pool, prID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", worker.PoisonError(fmt.Errorf("pr %d not found", prID))
		}
		return "", err
	}
	ownerRow, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, pool, pr.HeadRepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", worker.PoisonError(fmt.Errorf("pr %d: repo %d not found", prID, pr.HeadRepoID))
		}
		return "", fmt.Errorf("load repo owner: %w", err)
	}
	ownerSlug, err := ownerSlugString(ownerRow.OwnerUsername)
	if err != nil {
		return "", worker.PoisonError(fmt.Errorf("pr %d: repo owner slug: %w", prID, err))
	}
	return rfs.RepoPath(ownerSlug, ownerRow.RepoName)
}

// enqueuePRActionsTrigger fans out a workflow:trigger job for a PR
// state transition (action ∈ {"opened", "synchronize"} for v1).
//
// Trust gate: the trigger handler evaluates the PR author through
// internal/auth/policy. Trusted same-repo collaborators run immediately;
// untrusted PRs are queued in an approval-required state when policy allows.
func enqueuePRActionsTrigger(ctx context.Context, deps PRJobsDeps, prID int64, action string) error {
	pr, err := pullsdb.New().GetPullRequestByIssueID(ctx, deps.Pool, prID)
	if err != nil {
		return fmt.Errorf("load pr: %w", err)
	}
	issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, pr.IssueID)
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, pr.HeadRepoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}

	if !issue.AuthorUserID.Valid {
		deps.Logger.InfoContext(ctx, "pr: skipping workflow:trigger (missing author)",
			"pr_id", prID, "action", action)
		return nil
	}

	ownerLogin, err := resolvePRActionsOwnerLogin(ctx, deps.Pool, repo)
	if err != nil {
		return fmt.Errorf("load owner: %w", err)
	}
	gitDir, err := deps.RepoFS.RepoPath(ownerLogin, repo.Name)
	if err != nil {
		return fmt.Errorf("repo path: %w", err)
	}

	// Changed paths: head_oid against base_oid for paths: filter
	// evaluation. Best-effort — if the diff fails (e.g., tip pruned)
	// we proceed without paths, and paths-filtered workflows won't
	// trigger.
	changed, err := gitops.ChangedPaths(ctx, gitDir, pr.BaseOid, pr.HeadOid)
	if err != nil {
		deps.Logger.WarnContext(ctx, "pr: changed-paths failed",
			"pr_id", prID, "error", err)
		changed = nil
	}

	authorLogin := ""
	if u, err := usersdb.New().GetUserByID(ctx, deps.Pool, issue.AuthorUserID.Int64); err == nil {
		authorLogin = u.Username
	}
	payload := actionsevent.PullRequest(
		action, issue.Number, issue.Title,
		actionsevent.PRRef{Ref: pr.HeadRef, SHA: pr.HeadOid},
		actionsevent.PRRef{Ref: pr.BaseRef, SHA: pr.BaseOid},
		authorLogin,
	)

	job := trigger.JobPayload{
		RepoID:         repo.ID,
		HeadSHA:        pr.HeadOid,
		HeadRef:        "refs/heads/" + pr.HeadRef,
		EventKind:      trigger.EventPullRequest,
		EventPayload:   payload,
		ActorUserID:    issue.AuthorUserID.Int64,
		TriggerEventID: fmt.Sprintf("pr_%s:%d:%s", action, prID, pr.HeadOid),
		Action:         action,
		BaseRef:        pr.BaseRef,
		HeadRefShort:   pr.HeadRef,
		ChangedPaths:   changed,
	}
	if _, err := worker.Enqueue(ctx, deps.Pool, trigger.KindWorkflowTrigger, job, worker.EnqueueOptions{}); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

func resolvePRActionsOwnerLogin(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo) (string, error) {
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
