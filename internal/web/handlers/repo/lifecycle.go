// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// MountLifecycle registers the repo settings danger-zone routes plus
// the per-user inbox/restore views. Caller wraps with RequireUser so
// every route below has a logged-in viewer in context. Routes:
//
//	GET  /{owner}/{repo}/settings              — danger zone view
//	POST /{owner}/{repo}/settings/rename
//	POST /{owner}/{repo}/settings/transfer
//	POST /{owner}/{repo}/settings/archive
//	POST /{owner}/{repo}/settings/unarchive
//	POST /{owner}/{repo}/settings/visibility
//	POST /{owner}/{repo}/settings/delete
//	POST /transfers/{id}/accept
//	POST /transfers/{id}/decline
//	POST /transfers/{id}/cancel
//	GET  /transfers                            — recipient inbox
//	GET  /settings/repositories                — restore listing
//	POST /settings/repositories/restore/{id}
func (h *Handlers) MountLifecycle(r chi.Router) {
	r.Get("/{owner}/{repo}/settings", h.repoSettings)
	r.Post("/{owner}/{repo}/settings/rename", h.repoRename)
	r.Post("/{owner}/{repo}/settings/transfer", h.repoTransferRequest)
	r.Post("/{owner}/{repo}/settings/archive", h.repoArchive)
	r.Post("/{owner}/{repo}/settings/unarchive", h.repoUnarchive)
	r.Post("/{owner}/{repo}/settings/visibility", h.repoVisibility)
	r.Post("/{owner}/{repo}/settings/delete", h.repoSoftDelete)
	r.Post("/transfers/{id}/accept", h.transferAccept)
	r.Post("/transfers/{id}/decline", h.transferDecline)
	r.Post("/transfers/{id}/cancel", h.transferCancel)
	r.Get("/transfers", h.transferInbox)
	r.Get("/settings/repositories", h.restoreList)
	r.Post("/settings/repositories/restore/{id}", h.repoRestore)
}

// loadRepoAndAuthorize is the common preamble for every settings-route
// handler. Resolves owner+repo, applies policy.Can with the chosen
// action, and either returns the row or writes the response.
func (h *Handlers) loadRepoAndAuthorize(w http.ResponseWriter, r *http.Request, action policy.Action) (reposdb.Repo, usersdb.User, bool) {
	ownerName := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	owner, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, ownerName)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	row, err := h.rq.GetRepoByOwnerUserAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:        repoName,
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	repoRef := policy.NewRepoRefFromRepo(row)
	dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, action, repoRef)
	if !dec.Allow {
		// Maybe404 picks 404 for "shouldn't know it exists" denials
		// (private + non-collab) and 403 for honest "you can't do that
		// to a repo you can see" denials (owner pushing to archived).
		// Without this we leaked existence by always 404'ing — fixed
		// per S00-S25 audit, finding H7.
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	return row, owner, true
}

// repoSettings renders the danger-zone view. Limited to owners (admin
// action). The actual settings UI is S32; this is just the danger
// zone S16 owns.
func (h *Handlers) repoSettings(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	transfers, _ := h.rq.ListTransfersForRepo(r.Context(), h.d.Pool, row.ID)
	h.d.Render.RenderPage(w, r, "repo/settings", map[string]any{
		"Title":     "Settings · " + row.Name,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner.Username,
		"Repo":      row,
		"Transfers": transfers,
	})
}

func (h *Handlers) repoRename(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	newName := strings.ToLower(strings.TrimSpace(r.PostFormValue("new_name")))
	viewer := middleware.CurrentUserFromContext(r.Context())
	err := lifecycle.Rename(r.Context(), h.lifecycleDeps(), lifecycle.RenameParams{
		ActorUserID: viewer.ID,
		RepoID:      row.ID,
		OwnerUserID: owner.ID,
		OwnerName:   owner.Username,
		OldName:     row.Name,
		NewName:     newName,
	})
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	http.Redirect(w, r, "/"+owner.Username+"/"+newName+"/settings", http.StatusSeeOther)
}

func (h *Handlers) repoTransferRequest(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.PostFormValue("to_user")))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	if confirm != owner.Username+"/"+row.Name {
		http.Error(w, "confirmation text didn't match owner/repo", http.StatusBadRequest)
		return
	}
	to, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, target)
	if err != nil {
		http.Error(w, "recipient not found", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := lifecycle.RequestTransfer(r.Context(), h.lifecycleDeps(), lifecycle.TransferRequestParams{
		ActorUserID: viewer.ID, RepoID: row.ID,
		FromUserID: owner.ID, ToPrincipalKind: "user", ToPrincipalID: to.ID,
	}); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=transfer-requested", http.StatusSeeOther)
}

func (h *Handlers) repoArchive(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoArchive)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.Archive(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=archived", http.StatusSeeOther)
}

func (h *Handlers) repoUnarchive(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoArchive)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.Unarchive(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=unarchived", http.StatusSeeOther)
}

func (h *Handlers) repoVisibility(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoVisibility)
	if !ok {
		return
	}
	to := strings.ToLower(strings.TrimSpace(r.PostFormValue("visibility")))
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.SetVisibility(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID, to); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=visibility", http.StatusSeeOther)
}

func (h *Handlers) repoSoftDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoDelete)
	if !ok {
		return
	}
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	if confirm != owner.Username+"/"+row.Name {
		http.Error(w, "confirmation text didn't match owner/repo", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.SoftDelete(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/repositories?notice=deleted", http.StatusSeeOther)
}

// transferAccept / Decline / Cancel act on a transfer ID. Recipient is
// always the actor for accept/decline; sender (or repo admin) for cancel.
func (h *Handlers) transferAccept(w http.ResponseWriter, r *http.Request) {
	id := parseTransferID(w, r)
	if id == 0 {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.AcceptTransfer(r.Context(), h.lifecycleDeps(), viewer.ID, id); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/transfers?notice=accepted", http.StatusSeeOther)
}

func (h *Handlers) transferDecline(w http.ResponseWriter, r *http.Request) {
	id := parseTransferID(w, r)
	if id == 0 {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.DeclineTransfer(r.Context(), h.lifecycleDeps(), viewer.ID, id); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/transfers?notice=declined", http.StatusSeeOther)
}

func (h *Handlers) transferCancel(w http.ResponseWriter, r *http.Request) {
	id := parseTransferID(w, r)
	if id == 0 {
		return
	}
	// Verify the actor is allowed to cancel: must be repo admin on the
	// repo this transfer belongs to.
	row, err := h.rq.GetTransferRequest(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	repo, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, row.RepoID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	repoRef := policy.NewRepoRefFromRepo(repo)
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoAdmin, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return
	}
	if err := lifecycle.CancelTransfer(r.Context(), h.lifecycleDeps(), viewer.ID, id); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+chi.URLParam(r, "owner")+"/"+chi.URLParam(r, "repo")+"/settings?notice=transfer-canceled", http.StatusSeeOther)
}

func (h *Handlers) transferInbox(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.rq.ListPendingTransfersForUser(r.Context(), h.d.Pool, viewer.ID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.d.Render.RenderPage(w, r, "repo/transfers_inbox", map[string]any{
		"Title":     "Transfer inbox",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Pending":   rows,
	})
}

func (h *Handlers) restoreList(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.rq.ListSoftDeletedReposForOwner(r.Context(), h.d.Pool, pgtype.Int8{Int64: viewer.ID, Valid: true})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.d.Render.RenderPage(w, r, "repo/restore_list", map[string]any{
		"Title":     "Restore deleted repositories",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Repos":     rows,
	})
}

func (h *Handlers) repoRestore(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	repoID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || repoID <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	repo, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, repoID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !repo.OwnerUserID.Valid || repo.OwnerUserID.Int64 != viewer.ID {
		// Restore is owner-only; site-admin path is S34. Existence-leak
		// guard: don't reveal a soft-deleted repo to non-owners.
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := lifecycle.Restore(r.Context(), h.lifecycleDeps(), viewer.ID, repoID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/repositories?notice=restored", http.StatusSeeOther)
}

// parseTransferID parses the chi URL param; writes a 400 and returns 0
// on malformed input.
func parseTransferID(w http.ResponseWriter, r *http.Request) int64 {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return 0
	}
	return id
}

// lifecycleDeps assembles the orchestrator deps from the handler's deps.
func (h *Handlers) lifecycleDeps() lifecycle.Deps {
	return lifecycle.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Audit:  h.d.Audit,
		Logger: h.d.Logger,
	}
}

// lifecycleError surfaces the typed lifecycle errors as user-friendly
// HTTP responses. Specific cases get specific status codes; everything
// else falls back to 500.
func (h *Handlers) lifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrNameTaken),
		errors.Is(err, lifecycle.ErrInvalidName),
		errors.Is(err, lifecycle.ErrReservedName),
		errors.Is(err, lifecycle.ErrSameName),
		errors.Is(err, lifecycle.ErrInvalidVisibility),
		errors.Is(err, lifecycle.ErrTransferToSelf):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, lifecycle.ErrRenameRateLimited):
		http.Error(w, "rename rate limit (5 per 30 days) exceeded", http.StatusTooManyRequests)
	case errors.Is(err, lifecycle.ErrAlreadyArchived),
		errors.Is(err, lifecycle.ErrNotArchived),
		errors.Is(err, lifecycle.ErrAlreadyDeleted),
		errors.Is(err, lifecycle.ErrNotDeleted):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, lifecycle.ErrTransferTerminal),
		errors.Is(err, lifecycle.ErrTransferExpired):
		http.Error(w, "transfer no longer pending", http.StatusConflict)
	case errors.Is(err, lifecycle.ErrPastGrace):
		http.Error(w, "soft-delete grace expired", http.StatusGone)
	default:
		h.d.Logger.WarnContext(r.Context(), "lifecycle: unexpected error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
