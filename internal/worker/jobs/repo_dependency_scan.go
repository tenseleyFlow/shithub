// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos/dependencies"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type RepoDependencyScanDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

type RepoDependencyScanPayload struct {
	RepoID int64 `json:"repo_id"`
}

// RepoDependencyScan builds the supported dependency inventory for a
// repository's default branch and refreshes dependency alerts from the
// local advisory catalog. It is idempotent for the same default-branch
// head; removed packages are marked stale instead of deleted so alert
// history survives.
func RepoDependencyScan(deps RepoDependencyScanDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("repo dependency scan: missing pool")
		}
		if deps.RepoFS == nil {
			return errors.New("repo dependency scan: missing repo fs")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		var p RepoDependencyScanPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.RepoID == 0 {
			return worker.PoisonError(errors.New("missing repo_id"))
		}

		rq := reposdb.New()
		repo, err := rq.GetRepoByID(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("repo %d not found", p.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}
		if repo.DeletedAt.Valid {
			return nil
		}

		owner, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, repo.ID)
		if err != nil {
			return fmt.Errorf("load repo owner: %w", err)
		}
		ownerSlug, err := ownerSlugString(owner.OwnerUsername)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo owner slug: %w", err))
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerSlug, repo.Name)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo path: %w", err))
		}

		snapshot, err := dependencies.Build(ctx, gitDir, dependencies.BuildOptions{
			Ref: repo.DefaultBranch,
		})
		if err != nil {
			return fmt.Errorf("build dependency inventory: %w", err)
		}
		head := snapshot.HeadSHA
		if head == "" {
			head = "empty"
		}
		if _, err := rq.UpsertRepoDependencySnapshot(ctx, deps.Pool, reposdb.UpsertRepoDependencySnapshotParams{
			RepoID:          repo.ID,
			DefaultBranch:   repo.DefaultBranch,
			HeadSha:         head,
			ManifestCount:   clampInt32(len(snapshot.Manifests)),
			DependencyCount: clampInt32(len(snapshot.Dependencies)),
		}); err != nil {
			return fmt.Errorf("upsert dependency snapshot: %w", err)
		}

		for _, dep := range snapshot.Dependencies {
			if _, err := rq.UpsertRepoDependency(ctx, deps.Pool, reposdb.UpsertRepoDependencyParams{
				RepoID:         repo.ID,
				Ecosystem:      dep.Ecosystem,
				PackageName:    dep.PackageName,
				PackageVersion: dep.PackageVersion,
				ManifestPath:   dep.ManifestPath,
				LockfilePath:   dep.LockfilePath,
				Scope:          dep.Scope,
				Direct:         dep.Direct,
				PackageManager: dep.PackageManager,
				Source:         dep.Source,
				LastSeenSha:    head,
			}); err != nil {
				logger.WarnContext(ctx, "repo dependency scan: upsert dependency",
					"repo_id", repo.ID, "ecosystem", dep.Ecosystem, "package", dep.PackageName, "error", err)
			}
		}
		if err := rq.MarkRepoDependenciesStale(ctx, deps.Pool, reposdb.MarkRepoDependenciesStaleParams{
			RepoID:      repo.ID,
			LastSeenSha: head,
		}); err != nil {
			logger.WarnContext(ctx, "repo dependency scan: mark stale", "repo_id", repo.ID, "error", err)
		}
		if err := rq.RefreshDependencyAlertsForRepo(ctx, deps.Pool, repo.ID); err != nil {
			logger.WarnContext(ctx, "repo dependency scan: refresh alerts", "repo_id", repo.ID, "error", err)
		}
		if err := rq.ResolveStaleDependencyAlertsForRepo(ctx, deps.Pool, repo.ID); err != nil {
			logger.WarnContext(ctx, "repo dependency scan: resolve stale alerts", "repo_id", repo.ID, "error", err)
		}
		logger.InfoContext(ctx, "repo dependency scan complete",
			"repo_id", repo.ID, "head_sha", head,
			"manifests", len(snapshot.Manifests), "dependencies", len(snapshot.Dependencies))
		return nil
	}
}

func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
