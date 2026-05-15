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

	"github.com/tenseleyFlow/shithub/internal/git/hooks"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// TemplateInitDeps mirrors ForkCloneDeps. ShithubdPath is forwarded
// to the hook installer so the new repo's pre-/post-receive shims
// resolve to the right binary; nil in tests.
type TemplateInitDeps struct {
	Pool         *pgxpool.Pool
	RepoFS       *storage.RepoFS
	Logger       *slog.Logger
	ShithubdPath string
}

// TemplateInitPayload — the bare minimum to find the template on
// disk and the init_pending shell to populate.
type TemplateInitPayload struct {
	TemplateRepoID int64 `json:"template_repo_id"`
	NewRepoID      int64 `json:"new_repo_id"`
}

// RepoTemplateInit runs `git clone --bare` (NO --shared) so the new
// repo is independent of its template. Mirrors RepoForkClone's
// init_pending → initialized lifecycle. Unlike forks, there is no
// preciousObjects step on the template — the template's objects
// were copied (not referenced), so a future `git gc` on the
// template can't prune anything the new repo relies on.
//
// Permanent errors (template missing, target name conflict on disk,
// clone failure) flip the new repo's init_status to init_failed and
// return a poison error so the worker doesn't retry. Transient
// failures bubble up and the worker retries with backoff.
func RepoTemplateInit(deps TemplateInitDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p TemplateInitPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.TemplateRepoID == 0 || p.NewRepoID == 0 {
			return worker.PoisonError(errors.New("missing template_repo_id or new_repo_id"))
		}

		rq := reposdb.New()
		uq := usersdb.New()
		oq := orgsdb.New()

		newRepo, err := rq.GetRepoByID(ctx, deps.Pool, p.NewRepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("template-init repo %d not found", p.NewRepoID))
			}
			return err
		}
		// Idempotency: a retry after a successful run sees init_status
		// = initialized and does nothing.
		if newRepo.InitStatus == reposdb.RepoInitStatusInitialized {
			return nil
		}
		template, err := rq.GetRepoByID(ctx, deps.Pool, p.TemplateRepoID)
		if err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, fmt.Errorf("template %d not found", p.TemplateRepoID))
		}
		if template.DeletedAt.Valid {
			return failTemplateInit(ctx, deps, p.NewRepoID, errors.New("template soft-deleted between enqueue and clone"))
		}
		if !template.IsTemplate {
			return failTemplateInit(ctx, deps, p.NewRepoID, errors.New("source repo is no longer marked as a template"))
		}

		newOwner, err := getOwnerSlug(ctx, deps.Pool, uq, oq, newRepo)
		if err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, err)
		}
		templateOwner, err := getOwnerSlug(ctx, deps.Pool, uq, oq, template)
		if err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, err)
		}
		newPath, err := deps.RepoFS.RepoPath(newOwner, newRepo.Name)
		if err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, err)
		}
		templatePath, err := deps.RepoFS.RepoPath(templateOwner, template.Name)
		if err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, err)
		}

		if err := deps.RepoFS.CloneBareIndependent(ctx, templatePath, newPath); err != nil {
			return failTemplateInit(ctx, deps, p.NewRepoID, fmt.Errorf("clone: %w", err))
		}

		// Install push-pipeline hooks so subsequent user pushes fire
		// push:process. Same install as the fork worker.
		if deps.ShithubdPath != "" {
			if err := hooks.Install(newPath, deps.ShithubdPath); err != nil {
				return failTemplateInit(ctx, deps, p.NewRepoID, fmt.Errorf("install hooks: %w", err))
			}
		}

		if err := rq.SetRepoInitStatus(ctx, deps.Pool, reposdb.SetRepoInitStatusParams{
			ID: newRepo.ID, InitStatus: reposdb.RepoInitStatusInitialized,
		}); err != nil {
			return err
		}
		return nil
	}
}

// failTemplateInit flips init_status to init_failed and returns a
// poison error. Same shape as failFork.
func failTemplateInit(ctx context.Context, deps TemplateInitDeps, newRepoID int64, cause error) error {
	if err := reposdb.New().SetRepoInitStatus(ctx, deps.Pool, reposdb.SetRepoInitStatusParams{
		ID: newRepoID, InitStatus: reposdb.RepoInitStatusInitFailed,
	}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "template-init: mark init_failed", "new_repo_id", newRepoID, "error", err)
	}
	return worker.PoisonError(cause)
}
