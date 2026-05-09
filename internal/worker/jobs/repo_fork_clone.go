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

	"github.com/tenseleyFlow/shithub/internal/git/hooks"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// ForkCloneDeps wires the job. Same shape as the rest of the fork-
// adjacent jobs so the worker registration is uniform.
//
// ShithubdPath is the absolute path to the shithubd binary, used by
// the hook installer to point the fork's pre-/post-receive hooks at
// the right binary. Empty in tests that don't exercise the hook
// stack — the clone still runs but the fork won't fire push:process
// on subsequent user pushes.
type ForkCloneDeps struct {
	Pool         *pgxpool.Pool
	RepoFS       *storage.RepoFS
	Logger       *slog.Logger
	ShithubdPath string
}

// ForkClonePayload — the bare minimum to find the source on disk and
// the fork shell to populate. Both ids are looked up at job time so
// soft-deletion between enqueue and run is detected (we set
// init_failed and stop).
type ForkClonePayload struct {
	SourceRepoID int64 `json:"source_repo_id"`
	ForkRepoID   int64 `json:"fork_repo_id"`
}

// RepoForkClone runs `git clone --bare --shared` for a fork shell,
// then sets `extensions.preciousObjects = true` on the source so a
// future `git gc` on the source can't prune objects the fork
// reaches via alternates.
//
// On any permanent error (source missing, target name conflict on
// disk, clone failure) the job flips the fork's init_status to
// init_failed and returns a poison error so the worker doesn't
// retry. Transient failures (DB blip mid-update) leave init_pending
// and bubble up so the worker queue retries with backoff.
func RepoForkClone(deps ForkCloneDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p ForkClonePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.SourceRepoID == 0 || p.ForkRepoID == 0 {
			return worker.PoisonError(errors.New("missing source_repo_id or fork_repo_id"))
		}

		rq := reposdb.New()
		uq := usersdb.New()

		fork, err := rq.GetRepoByID(ctx, deps.Pool, p.ForkRepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("fork %d not found", p.ForkRepoID))
			}
			return err
		}
		// Idempotency: a retry after a successful run sees init_status
		// = initialized and does nothing. The on-disk clone is what
		// we'd want to re-do; we trust that an initialized row means
		// the disk side already succeeded.
		if fork.InitStatus == reposdb.RepoInitStatusInitialized {
			return nil
		}
		source, err := rq.GetRepoByID(ctx, deps.Pool, p.SourceRepoID)
		if err != nil {
			return failFork(ctx, deps, p.ForkRepoID, fmt.Errorf("source %d not found", p.SourceRepoID))
		}
		if source.DeletedAt.Valid {
			return failFork(ctx, deps, p.ForkRepoID, errors.New("source repo soft-deleted between enqueue and clone"))
		}

		forkOwner, err := getOwnerUsername(ctx, deps.Pool, uq, fork)
		if err != nil {
			return failFork(ctx, deps, p.ForkRepoID, err)
		}
		sourceOwner, err := getOwnerUsername(ctx, deps.Pool, uq, source)
		if err != nil {
			return failFork(ctx, deps, p.ForkRepoID, err)
		}
		forkPath, err := deps.RepoFS.RepoPath(forkOwner, fork.Name)
		if err != nil {
			return failFork(ctx, deps, p.ForkRepoID, err)
		}
		sourcePath, err := deps.RepoFS.RepoPath(sourceOwner, source.Name)
		if err != nil {
			return failFork(ctx, deps, p.ForkRepoID, err)
		}

		if err := deps.RepoFS.CloneBareShared(ctx, sourcePath, forkPath); err != nil {
			// If the dst already exists with content we treat as
			// poison (something is fundamentally wrong with our
			// state — repo row exists but disk has unrelated content).
			return failFork(ctx, deps, p.ForkRepoID, fmt.Errorf("clone: %w", err))
		}

		// Install push-pipeline hooks on the fork so subsequent
		// user pushes fire push:process. Same install as the
		// synchronous repo create path (internal/repos/create.go).
		// Skipped in tests that don't pass ShithubdPath.
		if deps.ShithubdPath != "" {
			if err := hooks.Install(forkPath, deps.ShithubdPath); err != nil {
				return failFork(ctx, deps, p.ForkRepoID, fmt.Errorf("install hooks: %w", err))
			}
		}

		// Source GC safety: pin source's objects so future `git gc`
		// can't prune what the fork reaches via alternates.
		// Idempotent — running the config twice is a no-op.
		if err := deps.RepoFS.SetPreciousObjects(ctx, sourcePath); err != nil {
			// Not fatal for the fork — the alternates link still
			// works today. Log loudly so an operator can investigate.
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "fork: set preciousObjects on source",
					"source_repo_id", source.ID, "error", err)
			}
		}

		if err := rq.SetRepoInitStatus(ctx, deps.Pool, reposdb.SetRepoInitStatusParams{
			ID: fork.ID, InitStatus: reposdb.RepoInitStatusInitialized,
		}); err != nil {
			return err
		}
		return nil
	}
}

// failFork flips the fork's init_status to init_failed and returns
// a worker.PoisonError so the failure isn't retried. The repo row
// stays in the DB so the user can see the failure and choose to
// hard-delete; we don't auto-cleanup because that risks racing a
// concurrent retry.
func failFork(ctx context.Context, deps ForkCloneDeps, forkRepoID int64, cause error) error {
	if err := reposdb.New().SetRepoInitStatus(ctx, deps.Pool, reposdb.SetRepoInitStatusParams{
		ID: forkRepoID, InitStatus: reposdb.RepoInitStatusInitFailed,
	}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "fork: mark init_failed", "fork_repo_id", forkRepoID, "error", err)
	}
	return worker.PoisonError(cause)
}

// getOwnerUsername — same shape as the resolver in internal/repos/
// fork/helpers.go but kept local to avoid pulling the orchestrator
// package into the worker import graph.
func getOwnerUsername(ctx context.Context, pool *pgxpool.Pool, uq *usersdb.Queries, repo reposdb.Repo) (string, error) {
	if !repo.OwnerUserID.Valid {
		return "", fmt.Errorf("repo %d has no user owner (org-owned arrives in S31)", repo.ID)
	}
	u, err := uq.GetUserByID(ctx, pool, repo.OwnerUserID.Int64)
	if err != nil {
		return "", fmt.Errorf("load owner: %w", err)
	}
	return u.Username, nil
}
