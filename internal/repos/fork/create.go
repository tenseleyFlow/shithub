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
	SourceRepoID int64
	ActorUserID  int64
	// TargetOwnerKind is "user" or "org". Empty defaults to "user"
	// for back-compat with the pre-picker call sites.
	TargetOwnerKind string
	// TargetOwnerID is the id of the user or org the fork lands
	// under. Authorization (viewer is allowed to create repos
	// under this owner) is the caller's responsibility — the
	// orchestrator just maps the kind+id to the right INSERT.
	TargetOwnerID int64
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

	// Normalize kind. Empty defaults to "user" — keeps pre-picker
	// callers (`TargetOwnerID` populated, no kind) working.
	targetKind := p.TargetOwnerKind
	if targetKind == "" {
		targetKind = "user"
	}
	if targetKind != "user" && targetKind != "org" {
		return CreateResult{}, fmt.Errorf("fork: invalid TargetOwnerKind %q", targetKind)
	}

	// Same-owner-name shortcut: a fork that points back at its own
	// owner with the same name is a no-op. Detect for both user-
	// and org-owned sources; the form requires a rename in both
	// cases.
	switch {
	case targetKind == "user" && source.OwnerUserID.Valid &&
		source.OwnerUserID.Int64 == p.TargetOwnerID && targetName == source.Name:
		return CreateResult{}, ErrSelfForkSameName
	case targetKind == "org" && source.OwnerOrgID.Valid &&
		source.OwnerOrgID.Int64 == p.TargetOwnerID && targetName == source.Name:
		return CreateResult{}, ErrSelfForkSameName
	}

	visibility, ok := allowedTargetVisibility(string(source.Visibility), p.TargetVisibility)
	if !ok {
		return CreateResult{}, ErrVisibilityFloor
	}

	// Check name availability against the target owner.
	var exists bool
	switch targetKind {
	case "user":
		exists, err = rq.ExistsRepoForOwnerUser(ctx, deps.Pool, reposdb.ExistsRepoForOwnerUserParams{
			OwnerUserID: pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
			Name:        targetName,
		})
	case "org":
		exists, err = rq.ExistsRepoForOwnerOrg(ctx, deps.Pool, reposdb.ExistsRepoForOwnerOrgParams{
			OwnerOrgID: pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
			Name:       targetName,
		})
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("fork: name check: %w", err)
	}
	if exists {
		return CreateResult{}, ErrTargetNameTaken
	}

	insertParams := reposdb.CreateForkRepoParams{
		Name:          targetName,
		Description:   source.Description,
		Visibility:    reposdb.RepoVisibility(visibility),
		DefaultBranch: source.DefaultBranch,
		ForkOfRepoID:  pgtype.Int8{Int64: source.ID, Valid: true},
	}
	switch targetKind {
	case "user":
		insertParams.OwnerUserID = pgtype.Int8{Int64: p.TargetOwnerID, Valid: true}
	case "org":
		insertParams.OwnerOrgID = pgtype.Int8{Int64: p.TargetOwnerID, Valid: true}
	}
	row, err := rq.CreateForkRepo(ctx, deps.Pool, insertParams)
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
