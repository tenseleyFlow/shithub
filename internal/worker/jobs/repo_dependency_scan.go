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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
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
		if enqueued, err := enqueueSecurityDependencyUpdateJobsForScan(ctx, deps.Pool, logger, repo); err != nil {
			logger.WarnContext(ctx, "repo dependency scan: enqueue security updates", "repo_id", repo.ID, "error", err)
		} else if enqueued > 0 {
			logger.InfoContext(ctx, "repo dependency scan enqueued security dependency updates",
				"repo_id", repo.ID, "jobs", enqueued)
		}
		logger.InfoContext(ctx, "repo dependency scan complete",
			"repo_id", repo.ID, "head_sha", head,
			"manifests", len(snapshot.Manifests), "dependencies", len(snapshot.Dependencies))
		return nil
	}
}

func enqueueSecurityDependencyUpdateJobsForScan(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, repo reposdb.Repo) (int, error) {
	if !repo.OwnerOrgID.Valid {
		return 0, nil
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: pool},
		billing.PrincipalForOrg(repo.OwnerOrgID.Int64),
		entitlements.FeatureDependabotSecurityUpdates)
	if err != nil {
		return 0, err
	}
	if !decision.Allowed {
		return 0, nil
	}

	rq := reposdb.New()
	configs, err := rq.ListEnabledDependencyUpdateConfigsForRepo(ctx, pool, repo.ID)
	if err != nil {
		return 0, err
	}
	if len(configs) == 0 {
		return 0, nil
	}
	alerts, err := rq.ListOpenDependencyAlertsForRepo(ctx, pool, repo.ID)
	if err != nil {
		return 0, err
	}
	if len(alerts) == 0 {
		return 0, nil
	}
	prs, err := rq.ListDependencyUpdatePRsForRepo(ctx, pool, repo.ID)
	if err != nil {
		return 0, err
	}
	if repoHasOpenSecurityDependencyUpdatePR(prs) {
		return 0, nil
	}

	enqueued := 0
	for _, cfg := range configs {
		if !dependencyUpdateConfigHasSecurityAlert(cfg, alerts) {
			continue
		}
		active, err := rq.CountActiveDependencyUpdateJobsForConfigKind(ctx, pool, reposdb.CountActiveDependencyUpdateJobsForConfigKindParams{
			ConfigID: pgtype.Int8{Int64: cfg.ID, Valid: true},
			JobKind:  "security_update",
		})
		if err != nil {
			return enqueued, err
		}
		if active > 0 {
			continue
		}

		if err := enqueueSecurityDependencyUpdateJobForConfig(ctx, pool, rq, repo, cfg); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "repo dependency scan: security update enqueue transaction failed",
					"repo_id", repo.ID, "config_id", cfg.ID)
			}
			return enqueued, err
		}
		enqueued++
	}
	if enqueued > 0 {
		if err := worker.Notify(ctx, pool); err != nil && logger != nil {
			logger.WarnContext(ctx, "repo dependency scan: dependency update notify failed", "repo_id", repo.ID, "error", err)
		}
	}
	return enqueued, nil
}

func enqueueSecurityDependencyUpdateJobForConfig(ctx context.Context, pool *pgxpool.Pool, rq *reposdb.Queries, repo reposdb.Repo, cfg reposdb.DependencyUpdateConfig) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	summary := []byte(`{"status":"queued","message":"security update queued from dependency scan"}`)
	job, err := rq.CreateDependencyUpdateJob(ctx, tx, reposdb.CreateDependencyUpdateJobParams{
		RepoID:        repo.ID,
		ConfigID:      pgtype.Int8{Int64: cfg.ID, Valid: true},
		JobKind:       "security_update",
		Status:        "queued",
		TriggerSource: "dependency_scan",
		ResultSummary: summary,
	})
	if err != nil {
		return err
	}
	if _, err := worker.Enqueue(ctx, tx, worker.KindRepoDependencyUpdateRun,
		RepoDependencyUpdateRunPayload{JobID: job.ID}, worker.EnqueueOptions{}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func dependencyUpdateConfigHasSecurityAlert(cfg reposdb.DependencyUpdateConfig, alerts []reposdb.ListOpenDependencyAlertsForRepoRow) bool {
	for _, alert := range alerts {
		if alert.Ecosystem == cfg.Ecosystem && manifestInDependencyUpdateDirectory(alert.ManifestPath, cfg.Directory) {
			return true
		}
	}
	return false
}

func repoHasOpenSecurityDependencyUpdatePR(prs []reposdb.DependencyUpdatePr) bool {
	for _, pr := range prs {
		if pr.Status != "open" {
			continue
		}
		if pr.UpdateKind == "security" {
			return true
		}
		var packages []dependencyUpdatePackageIO
		if err := json.Unmarshal(pr.PackageSet, &packages); err != nil {
			continue
		}
		for _, pkg := range packages {
			if pkg.UpdateKind == "security" {
				return true
			}
		}
	}
	return false
}

func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
