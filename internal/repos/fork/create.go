// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// CreateParams describes a fork-create request.
type CreateParams struct {
	SourceRepoID  int64
	ActorUserID   int64
	TargetOwnerID int64 // user id; org id support lands with S31
	// TargetName is optional. Empty defaults to the source repo's
	// name; non-empty must pass the same name validator as repo
	// create (the lookup against the existing rows surfaces a
	// taken-name error if needed).
	TargetName string
	// TargetVisibility is optional. Empty defaults to source
	// visibility; non-empty must pass `allowedTargetVisibility`.
	// (Public source can fork to private; private source is
	// pinned to private.)
	TargetVisibility string
}

// CreateResult is the inserted fork shell. The on-disk clone hasn't
// run yet; init_status is `init_pending`. The caller is expected to
// enqueue the `repo:fork_clone` worker job whose payload is
// {SourceRepoID, ForkRepoID}.
type CreateResult struct {
	Fork   reposdb.Repo
	Source reposdb.Repo
}

// Create writes the fork's `repos` row, applies the fork_count
// trigger, and emits the audit row. The on-disk clone is the
// caller's responsibility to enqueue (the web handler does this
// right after Create returns).
//
// Authorization (visibility on source, login, fork:create policy)
// is the caller's responsibility — this orchestrator trusts the
// handler's policy gate. Visibility floor + same-owner-name guards
// are enforced here because they're domain rules, not auth rules.
func Create(ctx context.Context, deps Deps, p CreateParams) (CreateResult, error) {
	if p.ActorUserID == 0 {
		return CreateResult{}, ErrNotLoggedIn
	}
	rq := reposdb.New()

	source, err := rq.GetRepoByID(ctx, deps.Pool, p.SourceRepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CreateResult{}, ErrSourceNotFound
		}
		return CreateResult{}, fmt.Errorf("fork: load source: %w", err)
	}
	if source.DeletedAt.Valid {
		return CreateResult{}, ErrSourceDeleted
	}
	if source.IsArchived {
		return CreateResult{}, ErrSourceArchived
	}

	targetName := strings.TrimSpace(p.TargetName)
	if targetName == "" {
		targetName = source.Name
	}
	// Same-owner-name shortcut: if the target owner == source owner
	// AND the name didn't change, this is a no-op fork onto itself.
	// Users CAN fork their own repos but must rename.
	if source.OwnerUserID.Valid && source.OwnerUserID.Int64 == p.TargetOwnerID && targetName == source.Name {
		return CreateResult{}, ErrSelfForkSameName
	}

	visibility, ok := allowedTargetVisibility(string(source.Visibility), p.TargetVisibility)
	if !ok {
		return CreateResult{}, ErrVisibilityFloor
	}

	// Check name availability against the target owner.
	exists, err := rq.ExistsRepoForOwnerUser(ctx, deps.Pool, reposdb.ExistsRepoForOwnerUserParams{
		OwnerUserID: pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
		Name:        targetName,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("fork: name check: %w", err)
	}
	if exists {
		return CreateResult{}, ErrTargetNameTaken
	}

	row, err := rq.CreateForkRepo(ctx, deps.Pool, reposdb.CreateForkRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
		Name:          targetName,
		Description:   source.Description,
		Visibility:    reposdb.RepoVisibility(visibility),
		DefaultBranch: source.DefaultBranch,
		ForkOfRepoID:  pgtype.Int8{Int64: source.ID, Valid: true},
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("fork: insert row: %w", err)
	}

	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, p.ActorUserID,
			audit.ActionRepoForked, audit.TargetRepo, row.ID,
			map[string]any{"source_repo_id": source.ID})
	}
	return CreateResult{Fork: row, Source: source}, nil
}
