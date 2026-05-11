// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"bytes"
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
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// IndexCodeDeps wires the job.
type IndexCodeDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// IndexCodePayload is enqueued by the push-process job (or the
// reconciler) when the default branch advances.
type IndexCodePayload struct {
	RepoID int64 `json:"repo_id"`
}

// Code-search size limits, per the spec:
//   - Files > maxFileBytes are skipped from content indexing (path
//     stays indexed regardless).
//   - Per-file indexed content is truncated to maxIndexBytes so the
//     trigram column doesn't bloat for huge text files.
const (
	maxFileBytes  = 256 * 1024 // 256 KiB
	maxIndexBytes = 64 * 1024  // 64 KiB
)

// RepoIndexCode walks the repo's default branch tree, indexes paths
// for every file and content for files that fit the size + textness
// gates, then atomically swaps the new index in via delete-then-
// insert in a single tx. Readers never see a partial index.
//
// Tracks `repos.last_indexed_oid` so the reconciler can detect drift.
func RepoIndexCode(deps IndexCodeDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p IndexCodePayload
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
			return err
		}
		if repo.DeletedAt.Valid {
			// Repo went away between enqueue and now. Nothing to do.
			return nil
		}
		owner, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, repo.ID)
		if err != nil {
			return err
		}
		ownerSlug, err := ownerSlugString(owner.OwnerUsername)
		if err != nil {
			return err
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerSlug, repo.Name)
		if err != nil {
			return err
		}

		ref := repo.DefaultBranch
		// Resolve the current OID for the default branch. If empty
		// (no commits yet) clear any prior index and bail.
		oid, err := repogit.ResolveRefOID(ctx, gitDir, ref)
		if err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				if err := clearRepoIndex(ctx, deps.Pool, repo.ID); err != nil {
					return err
				}
				_ = rq.SetLastIndexedOID(ctx, deps.Pool, reposdb.SetLastIndexedOIDParams{
					ID: repo.ID, LastIndexedOid: pgtype.Text{Valid: false},
				})
				return nil
			}
			return fmt.Errorf("resolve %s: %w", ref, err)
		}

		// Walk the tree.
		paths, err := repogit.ListAllPaths(ctx, gitDir, ref)
		if err != nil {
			return fmt.Errorf("ls-tree: %w", err)
		}

		// Read each blob; classify size + text. Skipped paths still
		// land in the path index (only content is gated by size).
		type indexed struct {
			path    string
			content []byte // empty if skipped from content index
		}
		entries := make([]indexed, 0, len(paths))
		for _, path := range paths {
			if shouldSkipPath(path) {
				continue
			}
			ent := indexed{path: path}
			blob, err := repogit.ReadBlobBytes(ctx, gitDir, ref, path, maxFileBytes+1)
			if err == nil && len(blob) <= maxFileBytes && isText(blob) {
				if len(blob) > maxIndexBytes {
					blob = blob[:maxIndexBytes]
				}
				ent.content = blob
			}
			entries = append(entries, ent)
		}

		// Atomic swap: delete + insert in one tx.
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback(ctx)
			}
		}()
		if _, err := tx.Exec(ctx,
			`DELETE FROM code_search_paths WHERE repo_id = $1`, repo.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM code_search_content WHERE repo_id = $1`, repo.ID); err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := tx.Exec(ctx, `
				INSERT INTO code_search_paths (repo_id, ref_name, path, tsv)
				VALUES ($1, $2, $3, to_tsvector('shithub_search', $3))
				ON CONFLICT DO NOTHING
			`, repo.ID, ref, e.path); err != nil {
				return fmt.Errorf("insert path %s: %w", e.path, err)
			}
			if e.content != nil {
				content := string(e.content)
				if _, err := tx.Exec(ctx, `
					INSERT INTO code_search_content
					    (repo_id, ref_name, path, content_tsv, content_trgm)
					VALUES ($1, $2, $3, to_tsvector('shithub_search', $4), $4)
					ON CONFLICT DO NOTHING
				`, repo.ID, ref, e.path, content); err != nil {
					return fmt.Errorf("insert content %s: %w", e.path, err)
				}
			}
		}
		if err := rq.SetLastIndexedOID(ctx, tx, reposdb.SetLastIndexedOIDParams{
			ID: repo.ID, LastIndexedOid: pgtype.Text{String: oid, Valid: true},
		}); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	}
}

// clearRepoIndex drops every code-search row for a repo. Used when
// the default branch is gone (deleted, or never created) so we
// don't leave stale rows behind.
func clearRepoIndex(ctx context.Context, pool *pgxpool.Pool, repoID int64) error {
	if _, err := pool.Exec(ctx,
		`DELETE FROM code_search_paths WHERE repo_id = $1`, repoID); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM code_search_content WHERE repo_id = $1`, repoID); err != nil {
		return err
	}
	return nil
}

// shouldSkipPath filters out paths the spec calls out as
// "skipped by default": vendor/, node_modules/, dist/, .git*. The
// `path:` operator (post-MVP) lets users opt into them.
func shouldSkipPath(p string) bool {
	if strings.HasPrefix(p, ".git") {
		return true
	}
	for _, prefix := range []string{"vendor/", "node_modules/", "dist/"} {
		if strings.HasPrefix(p, prefix) || strings.Contains(p, "/"+prefix) {
			return true
		}
	}
	return false
}

// isText is the same NUL-byte heuristic S17 uses for the blob view:
// any NUL in the first 8 KiB → binary; otherwise text. Cheap, good
// enough for code-search content gating.
func isText(b []byte) bool {
	limit := len(b)
	if limit > 8192 {
		limit = 8192
	}
	return bytes.IndexByte(b[:limit], 0) < 0
}
