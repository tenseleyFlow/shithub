// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// RepoSizeRecalcDeps wires the size-recalc handler.
type RepoSizeRecalcDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// RepoSizeRecalcPayload — { "repo_id": <int> }.
type RepoSizeRecalcPayload struct {
	RepoID int64 `json:"repo_id"`
}

// RepoSizeRecalc walks the bare-repo tree and updates
// repos.disk_used_bytes. Walked in pure Go (no shelling out to du) so
// we get a portable sum and don't have to wrangle stderr from a
// blocked subprocess.
//
// Concurrent runs may compute slightly different sizes if a push lands
// mid-walk; that's acceptable — the *last* one wins, and quotas (post-
// MVP) tolerate small drift.
func RepoSizeRecalc(deps RepoSizeRecalcDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p RepoSizeRecalcPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.RepoID == 0 {
			return worker.PoisonError(errors.New("missing repo_id"))
		}

		rq := reposdb.New()
		ownerRow, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("repo %d not found", p.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}

		gitDir, err := deps.RepoFS.RepoPath(ownerRow.OwnerUsername, ownerRow.RepoName)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo path: %w", err))
		}
		size, err := walkSize(ctx, gitDir)
		if err != nil {
			return fmt.Errorf("walk size: %w", err)
		}
		if err := rq.UpdateRepoDiskUsed(ctx, deps.Pool, reposdb.UpdateRepoDiskUsedParams{
			ID:            p.RepoID,
			DiskUsedBytes: size,
		}); err != nil {
			return fmt.Errorf("update disk_used: %w", err)
		}
		return nil
	}
}

// walkSize sums the byte size of every regular file under root. Walks
// once; doesn't follow symlinks (we never create any inside a bare
// repo). Honors ctx so a long-running walk on a giant repo can be
// cancelled by graceful shutdown.
func walkSize(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
