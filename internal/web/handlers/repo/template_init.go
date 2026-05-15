// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/templateinit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// newRepoFromTemplate is the create-from-template branch of POST /new.
// Mirrors newRepoSubmit's structure (owner picker → orchestrator →
// worker enqueue) but uses templateinit.Create and dispatches the
// repo:template_init worker job. License/Gitignore/Readme are NOT
// applied — the template's tree initializes the new repo.
func (h *Handlers) newRepoFromTemplate(w http.ResponseWriter, r *http.Request, user middleware.CurrentUser, rawTemplateID string) {
	templateRepoID, err := strconv.ParseInt(rawTemplateID, 10, 64)
	if err != nil || templateRepoID <= 0 {
		h.renderNewForm(w, r, formState{}, "Invalid template repository.")
		return
	}
	// We re-parse the same form fields newRepoSubmit reads so the
	// re-render-on-error path round-trips the user's input.
	form := formState{
		Owner:       strings.TrimSpace(r.PostFormValue("owner")),
		Name:        repos.NormalizeName(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Visibility:  strings.TrimSpace(r.PostFormValue("visibility")),
	}
	if form.Visibility == "" {
		form.Visibility = "public"
	}

	// Load the template + authorize read.
	template, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, templateRepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.renderNewForm(w, r, form, "Template repository not found.")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "template-init: load template", "error", err, "template_repo_id", templateRepoID)
		h.renderNewForm(w, r, form, "Could not load the template repository.")
		return
	}
	if !template.IsTemplate {
		h.renderNewForm(w, r, form, "That repository is not marked as a template.")
		return
	}
	// Visibility-aware read check. Private templates are only visible
	// to actors with explicit read access. Reuse policy.Can with
	// ActionRepoRead.
	actor := middleware.CurrentUser{ID: user.ID, Username: user.Username, IsSiteAdmin: user.IsSiteAdmin}.PolicyActor()
	allowed := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoRead, policy.NewRepoRefFromRepo(template))
	if !allowed.Allow {
		// Existence-leak-safe: same message as not-found.
		h.renderNewForm(w, r, form, "Template repository not found.")
		return
	}

	// Resolve the target owner. Mirrors newRepoSubmit's branch.
	var targetKind string
	var targetOwnerID int64
	var targetOwnerSlug string
	if kind, id, ok := parseOwnerToken(form.Owner); ok && kind == "org" {
		odeps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
		org, oerr := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, id)
		if oerr != nil || org.DeletedAt.Valid {
			h.renderNewForm(w, r, form, "Selected organization not found.")
			return
		}
		isMem, _ := orgs.IsMember(r.Context(), odeps, org.ID, user.ID)
		if !isMem {
			h.renderNewForm(w, r, form, "You're not a member of that organization.")
			return
		}
		isOwner, _ := orgs.IsOwner(r.Context(), odeps, org.ID, user.ID)
		if !isOwner && !org.AllowMemberRepoCreate {
			h.renderNewForm(w, r, form, "This organization restricts repo creation to owners.")
			return
		}
		targetKind = "org"
		targetOwnerID = org.ID
		targetOwnerSlug = string(org.Slug)
	} else {
		targetKind = "user"
		targetOwnerID = user.ID
		targetOwnerSlug = user.Username
	}

	res, err := templateinit.Create(r.Context(), templateinit.Deps{
		Pool: h.d.Pool, Audit: h.d.Audit,
	}, templateinit.CreateParams{
		TemplateRepoID:    template.ID,
		ActorUserID:       user.ID,
		TargetOwnerKind:   targetKind,
		TargetOwnerID:     targetOwnerID,
		TargetName:        form.Name,
		TargetVisibility:  form.Visibility,
		TargetDescription: form.Description,
	})
	if err != nil {
		h.renderNewForm(w, r, form, friendlyTemplateInitError(err))
		return
	}

	if _, qerr := worker.Enqueue(
		r.Context(), h.d.Pool, worker.KindRepoTemplateInit,
		map[string]any{"template_repo_id": res.Template.ID, "new_repo_id": res.Repo.ID},
		worker.EnqueueOptions{},
	); qerr != nil {
		h.d.Logger.ErrorContext(r.Context(), "template-init: enqueue worker", "error", qerr, "new_repo_id", res.Repo.ID)
		// Don't block the redirect — the user sees the placeholder
		// page; an operator can re-queue from the failed-jobs table.
	}

	http.Redirect(w, r, "/"+targetOwnerSlug+"/"+res.Repo.Name, http.StatusSeeOther)
}

// friendlyTemplateInitError maps orchestrator-side errors to user-
// visible copy. Falls through to a generic message so internal-only
// errors don't leak.
func friendlyTemplateInitError(err error) string {
	switch {
	case errors.Is(err, templateinit.ErrSourceNotFound):
		return "Template repository not found."
	case errors.Is(err, templateinit.ErrSourceNotTemplate):
		return "That repository is not marked as a template."
	case errors.Is(err, templateinit.ErrSourceArchived):
		return "Template repository is archived."
	case errors.Is(err, templateinit.ErrSourceDeleted):
		return "Template repository was deleted."
	case errors.Is(err, templateinit.ErrTargetNameTaken):
		return "A repository with that name already exists for this owner."
	case errors.Is(err, templateinit.ErrVisibilityFloor):
		return "A private template can only produce private repositories."
	case errors.Is(err, repos.ErrInvalidName):
		return "Repository name must be 1–100 characters: lowercase letters, digits, hyphens, underscores, periods (no leading or trailing dot/hyphen)."
	}
	return "Could not create from template. Please try again."
}
