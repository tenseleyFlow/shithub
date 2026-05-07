// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// Archive sets is_archived=true on the repo. Idempotent in spirit — a
// double-archive returns ErrAlreadyArchived so handlers can surface a
// "this repo is already archived" friendly message without having to
// inspect state up front.
func Archive(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if repo.IsArchived {
		return ErrAlreadyArchived
	}
	if err := rq.ArchiveRepo(ctx, deps.Pool, repoID); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoArchived, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// Unarchive clears is_archived. Same idempotency contract as Archive.
func Unarchive(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if !repo.IsArchived {
		return ErrNotArchived
	}
	if err := rq.UnarchiveRepo(ctx, deps.Pool, repoID); err != nil {
		return fmt.Errorf("unarchive: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoUnarchived, audit.TargetRepo, repoID, nil)
	}
	return nil
}
