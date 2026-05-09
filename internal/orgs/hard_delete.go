// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
)

// pgInt8 wraps an int64 as the pgtype.Int8 the sqlc-generated query
// expects. Local helper so the call sites stay one-liners.
func pgInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }

// HardDeleteDeps is the augmented Deps shape the org hard-delete
// orchestrator needs. RepoFS lets us tear down the bare repos on
// disk; the lifecycle.HardDelete cascade reuses that same path so
// org-owned repos go through the same teardown user-owned ones do.
type HardDeleteDeps struct {
	Deps
	RepoFS *storage.RepoFS
	Audit  *audit.Recorder
}

// SoftDelete sets `orgs.deleted_at = now()`, kicking off the 14-day
// grace window. The principals trigger drops the slug row so the
// name becomes available to a new claimant immediately — restoring
// before grace re-creates the row. Pre-flight check: the org must
// not already be soft-deleted.
func SoftDelete(ctx context.Context, deps Deps, orgID, actorUserID int64) error {
	q := orgsdb.New()
	row, err := q.GetOrgByID(ctx, deps.Pool, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		return err
	}
	if row.DeletedAt.Valid {
		return ErrDeleted
	}
	if err := q.SoftDeleteOrg(ctx, deps.Pool, orgID); err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if deps.Audit != nil {
		// Borrow the repo soft-delete action shape until the audit
		// catalog grows a dedicated `org_soft_deleted` entry; the
		// kind=org metadata makes the org row identifiable in the log.
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoSoftDeleted,
			audit.TargetUser, orgID,
			map[string]any{"slug": string(row.Slug), "kind": "org"})
	}
	return nil
}

// Restore clears `deleted_at` for an org still inside its 14-day
// grace window. Caller (web handler) checks that the actor is an
// org owner BEFORE calling. Returns ErrOrgNotFound when the org's
// past-grace + already-purged or never existed.
func Restore(ctx context.Context, deps Deps, orgID, actorUserID int64) error {
	q := orgsdb.New()
	row, err := q.GetOrgByID(ctx, deps.Pool, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		return err
	}
	if !row.DeletedAt.Valid {
		// Not deleted; nothing to restore. Idempotent.
		return nil
	}
	// Slug-collision check: a new user/org may have claimed the slug
	// during the grace window (the principals row was dropped on
	// soft-delete). Refuse to restore if so — the operator can rename
	// the conflicting principal and retry.
	var taken bool
	if err := deps.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM principals WHERE slug = $1)`,
		row.Slug,
	).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrSlugTaken
	}
	if err := q.RestoreOrg(ctx, deps.Pool, orgID); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	_ = actorUserID // audit entry deferred
	return nil
}

// HardDelete is the cascade run by the past-grace sweep. For each
// org-owned repo (including already-soft-deleted ones) it runs the
// existing lifecycle.HardDelete tear-down (bare-repo on disk + DB
// rows). Then drops org_invitations + org_members + the orgs row.
// The principals trigger removes the slug entry as part of the
// orgs DELETE.
//
// Best-effort: a per-repo failure is logged but doesn't stop the
// sweep — leaving zombie repos in the DB is preferable to leaving
// the org row in a half-deleted state where its slug is permanently
// pinned. Operators can retry via the manual sweep job.
func HardDelete(ctx context.Context, hd HardDeleteDeps, orgID int64) error {
	q := orgsdb.New()
	repoIDs, err := q.ListOrgRepoIDs(ctx, hd.Pool, pgInt8(orgID))
	if err != nil {
		return fmt.Errorf("list org repos: %w", err)
	}
	ldeps := lifecycle.Deps{
		Pool: hd.Pool, RepoFS: hd.RepoFS, Audit: hd.Audit, Logger: hd.Logger,
	}
	for _, rid := range repoIDs {
		if err := lifecycle.HardDelete(ctx, ldeps, 0, rid); err != nil && hd.Logger != nil {
			hd.Logger.WarnContext(ctx, "orgs: cascade repo hard-delete failed",
				"org_id", orgID, "repo_id", rid, "error", err)
		}
	}
	// org_members + org_invitations cascade via FK ON DELETE CASCADE
	// already; the principals trigger drops the slug row inside the
	// same tx. Audit row goes in before the row vanishes so the log
	// retains the slug.
	row, _ := q.GetOrgByID(ctx, hd.Pool, orgID)
	if hd.Audit != nil {
		_ = hd.Audit.Record(ctx, hd.Pool, 0,
			audit.ActionRepoHardDeleted,
			audit.TargetUser, orgID,
			map[string]any{"slug": string(row.Slug), "kind": "org"})
	}
	if err := q.HardDeleteOrgRow(ctx, hd.Pool, orgID); err != nil {
		return fmt.Errorf("hard delete org row: %w", err)
	}
	return nil
}

// ListPastGraceOrgIDs is a thin wrapper exposing the sqlc query so
// the worker bundle doesn't need to import orgsdb directly.
func ListPastGraceOrgIDs(ctx context.Context, deps Deps) ([]int64, error) {
	return orgsdb.New().ListOrgIDsPastSoftDeleteGrace(ctx, deps.Pool)
}
