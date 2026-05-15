// SPDX-License-Identifier: AGPL-3.0-or-later

// Package templateinit orchestrates the "create from template" flow
// added in PRO-EXT01-06pre-b. Conceptually parallel to internal/repos/fork:
// inserts an init_pending row, returns the shell, and leaves the
// on-disk clone to the repo:template_init worker job.
//
// The orchestrator does NOT set fork_of_repo_id and does NOT bump the
// template's fork_count — a created-from-template repo is independent
// of its template (no alternates, no relationship).
package templateinit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// Errors mirror the fork package's surface so the calling handler can
// share error mapping. ErrSourceNotTemplate is template-specific —
// only repos with is_template=true can be used as a source.
var (
	ErrNotLoggedIn        = errors.New("templateinit: actor required")
	ErrSourceNotFound     = errors.New("templateinit: template not found")
	ErrSourceNotTemplate  = errors.New("templateinit: source is not a template repository")
	ErrSourceDeleted      = errors.New("templateinit: template repo soft-deleted")
	ErrSourceArchived     = errors.New("templateinit: template repo archived")
	ErrTargetNameTaken    = errors.New("templateinit: target name already exists for owner")
	ErrVisibilityFloor    = errors.New("templateinit: target visibility violates floor")
	ErrInvalidTargetKind  = errors.New("templateinit: invalid TargetOwnerKind")
	ErrUnreadableTemplate = errors.New("templateinit: template not readable by actor")
)

// Deps wires the orchestrator.
type Deps struct {
	Pool  reposdb.DBTX
	Audit *audit.Recorder
}

// CreateParams describes a create-from-template request.
type CreateParams struct {
	TemplateRepoID int64
	ActorUserID    int64
	// TargetOwnerKind is "user" or "org". Defaults to "user".
	TargetOwnerKind string
	// TargetOwnerID is the user or org the new repo lands under.
	// Authorization (viewer is allowed to create repos under this
	// owner) is the caller's responsibility.
	TargetOwnerID int64
	// TargetName is required (templates always rename — there's no
	// "same owner same name" case the way forks have).
	TargetName string
	// TargetVisibility: "public" or "private". Empty defaults to the
	// template's visibility. Same floor as forks: private template
	// → only private target.
	TargetVisibility string
	// TargetDescription is optional. Empty defaults to "" (NOT the
	// template's description — a fresh repo deserves a fresh blurb).
	TargetDescription string
}

// CreateResult carries the inserted shell + the template source.
// The on-disk clone is the worker's responsibility.
type CreateResult struct {
	Repo     reposdb.Repo
	Template reposdb.Repo
}

// Create writes the new-repo's row at init_status=init_pending and
// returns the shell. The caller enqueues the repo:template_init
// worker job with {TemplateRepoID, NewRepoID}.
//
// Authorization (visibility on template, login, repo:create on
// target owner) is the caller's responsibility — this orchestrator
// trusts the handler. Domain rules (template-flag check, name
// availability, visibility floor) are enforced here.
func Create(ctx context.Context, deps Deps, p CreateParams) (CreateResult, error) {
	if p.ActorUserID == 0 {
		return CreateResult{}, ErrNotLoggedIn
	}
	if strings.TrimSpace(p.TargetName) == "" {
		return CreateResult{}, fmt.Errorf("templateinit: TargetName required")
	}
	if err := repos.ValidateName(p.TargetName); err != nil {
		return CreateResult{}, err
	}
	rq := reposdb.New()

	template, err := rq.GetRepoByID(ctx, deps.Pool, p.TemplateRepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CreateResult{}, ErrSourceNotFound
		}
		return CreateResult{}, fmt.Errorf("templateinit: load template: %w", err)
	}
	if !template.IsTemplate {
		return CreateResult{}, ErrSourceNotTemplate
	}
	if template.DeletedAt.Valid {
		return CreateResult{}, ErrSourceDeleted
	}
	if template.IsArchived {
		return CreateResult{}, ErrSourceArchived
	}

	targetKind := p.TargetOwnerKind
	if targetKind == "" {
		targetKind = "user"
	}
	if targetKind != "user" && targetKind != "org" {
		return CreateResult{}, ErrInvalidTargetKind
	}

	visibility, ok := allowedTargetVisibility(string(template.Visibility), p.TargetVisibility)
	if !ok {
		return CreateResult{}, ErrVisibilityFloor
	}

	description := strings.TrimSpace(p.TargetDescription)
	if description != "" {
		if err := repos.ValidateDescription(description); err != nil {
			return CreateResult{}, err
		}
	}

	// Name availability for the target owner.
	switch targetKind {
	case "user":
		exists, err := rq.ExistsRepoForOwnerUser(ctx, deps.Pool, reposdb.ExistsRepoForOwnerUserParams{
			OwnerUserID: pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
			Name:        p.TargetName,
		})
		if err != nil {
			return CreateResult{}, fmt.Errorf("templateinit: name check: %w", err)
		}
		if exists {
			return CreateResult{}, ErrTargetNameTaken
		}
	case "org":
		exists, err := rq.ExistsRepoForOwnerOrg(ctx, deps.Pool, reposdb.ExistsRepoForOwnerOrgParams{
			OwnerOrgID: pgtype.Int8{Int64: p.TargetOwnerID, Valid: true},
			Name:       p.TargetName,
		})
		if err != nil {
			return CreateResult{}, fmt.Errorf("templateinit: name check: %w", err)
		}
		if exists {
			return CreateResult{}, ErrTargetNameTaken
		}
	}

	insert := reposdb.CreateRepoFromTemplateParams{
		Name:          p.TargetName,
		Description:   description,
		Visibility:    reposdb.RepoVisibility(visibility),
		DefaultBranch: template.DefaultBranch,
	}
	switch targetKind {
	case "user":
		insert.OwnerUserID = pgtype.Int8{Int64: p.TargetOwnerID, Valid: true}
	case "org":
		insert.OwnerOrgID = pgtype.Int8{Int64: p.TargetOwnerID, Valid: true}
	}
	row, err := rq.CreateRepoFromTemplate(ctx, deps.Pool, insert)
	if err != nil {
		return CreateResult{}, fmt.Errorf("templateinit: insert row: %w", err)
	}

	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, p.ActorUserID,
			audit.ActionRepoCreated, audit.TargetRepo, row.ID,
			map[string]any{"template_repo_id": template.ID, "kind": "template_init"})
	}
	return CreateResult{Repo: row, Template: template}, nil
}

// allowedTargetVisibility enforces the same floor as the fork
// orchestrator: a private template can only produce private new
// repos (the template's content is sensitive); a public template
// can produce either.
func allowedTargetVisibility(source, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return source, true
	}
	if target != "public" && target != "private" {
		return "", false
	}
	if source == "private" && target == "public" {
		return "", false
	}
	return target, true
}
