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

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
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
		// Chain a mergeability tick.
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindPRMergeability,
			map[string]any{"pr_id": p.PRID}, worker.EnqueueOptions{}); err != nil {
			deps.Logger.WarnContext(ctx, "pr:synchronize: enqueue mergeability", "pr_id", p.PRID, "error", err)
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

// PRMergePayload — enqueued from the POST .../merge handler. Contains
// the actor + chosen method + optional subject/body overrides.
type PRMergePayload struct {
	PRID        int64  `json:"pr_id"`
	ActorUserID int64  `json:"actor_user_id"`
	Method      string `json:"method"`
	Subject     string `json:"subject,omitempty"`
	Body        string `json:"body,omitempty"`
}

// PRMerge performs the requested merge strategy and updates DB state.
func PRMerge(deps PRJobsDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p PRMergePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.PRID == 0 || p.Method == "" {
			return worker.PoisonError(errors.New("missing pr_id or method"))
		}
		gitDir, err := resolveGitDirForPR(ctx, deps.Pool, deps.RepoFS, p.PRID)
		if err != nil {
			return err
		}
		return pulls.Merge(ctx, pulls.Deps{Pool: deps.Pool, Logger: deps.Logger}, pulls.MergeParams{
			PRID:        p.PRID,
			ActorUserID: p.ActorUserID,
			GitDir:      gitDir,
			Method:      p.Method,
			Subject:     p.Subject,
			Body:        p.Body,
		})
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
	repo, err := reposdb.New().GetRepoByID(ctx, pool, pr.HeadRepoID)
	if err != nil {
		return "", err
	}
	if !repo.OwnerUserID.Valid {
		return "", worker.PoisonError(fmt.Errorf("pr %d: repo has no owner user (org owners not yet supported here)", prID))
	}
	owner, err := usersdb.New().GetUserByID(ctx, pool, repo.OwnerUserID.Int64)
	if err != nil {
		return "", err
	}
	return rfs.RepoPath(owner.Username, repo.Name)
}
