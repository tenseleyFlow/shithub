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
	"github.com/tenseleyFlow/shithub/internal/repos/insights"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// RepoInsightsRecalcDeps wires the insights snapshot worker.
type RepoInsightsRecalcDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// RepoInsightsRecalcPayload is enqueued when a repo's default branch advances.
type RepoInsightsRecalcPayload struct {
	RepoID int64 `json:"repo_id"`
}

// RepoInsightsRecalc computes bounded git-history rollups and replaces
// the cached repo_insight_snapshots row. It is idempotent: rerunning
// against the same default-branch head writes the same snapshot shape.
func RepoInsightsRecalc(deps RepoInsightsRecalcDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p RepoInsightsRecalcPayload
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

		snapshot, err := insights.Build(ctx, gitDir, insights.BuildOptions{
			Ref:        repo.DefaultBranch,
			MaxCommits: insights.DefaultMaxCommits,
		})
		if err != nil {
			return fmt.Errorf("build insights: %w", err)
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("marshal insights: %w", err)
		}
		_, err = rq.UpsertRepoInsightSnapshot(ctx, deps.Pool, reposdb.UpsertRepoInsightSnapshotParams{
			RepoID:           repo.ID,
			DefaultBranch:    snapshot.DefaultBranch,
			HeadSha:          snapshot.HeadSHA,
			CommitCount:      int32(snapshot.CommitCount),
			ContributorCount: int32(snapshot.ContributorCount),
			Additions:        snapshot.Additions,
			Deletions:        snapshot.Deletions,
			Data:             data,
		})
		if err != nil {
			return fmt.Errorf("upsert insights: %w", err)
		}
		return nil
	}
}
