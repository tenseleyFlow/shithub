// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/fork"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// MountFork registers the fork-related routes:
//
//	GET  /{owner}/{repo}/forks       — paginated forks list (public)
//	POST /{owner}/{repo}/fork        — create a fork (auth-required)
//	POST /{owner}/{repo}/sync        — fast-forward sync from upstream (auth-required)
//
// The fork POST is auth-required + policy-gated by ActionForkCreate.
// The sync POST requires write on the fork.
func (h *Handlers) MountFork(r chi.Router) {
	r.Get("/{owner}/{repo}/forks", h.forksList)
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Post("/{owner}/{repo}/fork", h.repoFork)
		r.Post("/{owner}/{repo}/fork/retry", h.repoForkRetry)
		r.Get("/{owner}/{repo}/fork/check-name", h.repoForkCheckName)
		r.Post("/{owner}/{repo}/sync", h.repoSync)
	})
}

// forkDeps materializes a fork.Deps from the handler-set deps.
func (h *Handlers) forkDeps() fork.Deps {
	return fork.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Audit:  h.d.Audit,
		Logger: h.d.Logger,
	}
}

// repoFork handles POST /{owner}/{repo}/fork. Default behavior: fork
// the repo into the viewer's own namespace using the source's name
// (or the user-provided `target_name` form field). Source must be
// readable and forkable; target name + visibility floor are checked
// inside the orchestrator.
func (h *Handlers) repoFork(w http.ResponseWriter, r *http.Request) {
	ownerName := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	viewer := middleware.CurrentUserFromContext(r.Context())

	// Fork-create requires read on source AND login. The visibility
	// short-circuit at policy.Can step 4 covers anonymous-on-private;
	// step 9 covers anonymous-on-anything for fork:create.
	source, err := h.lookupRepoForViewer(r.Context(), ownerName, name, viewer)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	actor := viewer.PolicyActor()
	repoRef := policy.NewRepoRefFromRepo(source)
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionForkCreate, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}

	// Resolve fork target. The picker submits a single composite
	// `target_owner` token ("user:42" or "org:7") matching the
	// repo-create form pattern. Empty/missing token falls back to
	// the viewer's own user account (back-compat for any caller
	// that still POSTs without the picker).
	targetKind := "user"
	targetID := viewer.ID
	if tok := strings.TrimSpace(r.PostFormValue("target_owner")); tok != "" {
		kind, id, ok := parseOwnerToken(tok)
		if !ok {
			h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid target owner")
			return
		}
		targetKind, targetID = kind, id
	}
	// Authorize the target: viewer must own the user account or be
	// in the org's allow-create set. Re-derive the candidate list
	// here (don't trust the form) so a hand-crafted POST can't
	// fork into an org the viewer isn't a member of.
	targetOpt, ok := findForkTarget(h.ownerOptions(r), targetKind, targetID)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}

	res, err := fork.Create(r.Context(), h.forkDeps(), fork.CreateParams{
		SourceRepoID:      source.ID,
		ActorUserID:       viewer.ID,
		TargetOwnerKind:   targetKind,
		TargetOwnerID:     targetID,
		TargetName:        strings.TrimSpace(r.PostFormValue("target_name")),
		TargetVisibility:  strings.TrimSpace(r.PostFormValue("target_visibility")),
		TargetDescription: r.PostFormValue("target_description"),
	})
	if err != nil {
		h.handleForkError(w, r, err)
		return
	}
	// Enqueue the on-disk clone. The fork row exists with
	// init_status='init_pending' so the URL resolves immediately.
	if _, err := worker.Enqueue(
		r.Context(), h.d.Pool, worker.KindRepoForkClone,
		map[string]any{"source_repo_id": res.Source.ID, "fork_repo_id": res.Fork.ID},
		worker.EnqueueOptions{},
	); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "fork: enqueue clone", "error", err, "fork_id", res.Fork.ID)
	}
	// Emit a `forked` event for activity feeds (S26's domain_events
	// log). Public-flag follows source visibility, per the
	// per-event policy.
	_ = social.Emit(r.Context(), social.Deps{Pool: h.d.Pool}, social.EmitParams{
		ActorUserID: viewer.ID,
		Kind:        "forked",
		RepoID:      res.Fork.ID,
		SourceKind:  "repo",
		SourceID:    res.Source.ID,
		Public:      string(res.Source.Visibility) == "public",
	})
	// Auto-watch the new fork at level=all so the user sees fork-side
	// events (matches GitHub: the act of forking implies interest).
	_ = social.AutoWatchOnCollab(r.Context(), h.socialDeps(), viewer.ID, res.Fork.ID)
	http.Redirect(w, r, "/"+targetOpt.Slug+"/"+res.Fork.Name, http.StatusSeeOther)
}

// repoForkCheckName backs the modal's live name-availability preview.
// Returns a small JSON object the modal JS swaps into the status
// element next to the name input:
//
//	{"status":"available","message":"Name is available."}
//	{"status":"taken","message":"You already own a repository with that name."}
//	{"status":"invalid","message":"Name must be lowercase letters, digits, dot, dash, or underscore."}
//	{"status":"forbidden","message":"You can't fork into that owner."}
//
// Auth + policy mirror repoFork so a poking client can't enumerate
// repos under an org they don't belong to. The "available" answer is
// advisory — the POST handler is still authoritative, so a race
// between the check and the submit just turns the submit into a
// regular 409 with the existing taken-name banner.
func (h *Handlers) repoForkCheckName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ownerName := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	viewer := middleware.CurrentUserFromContext(ctx)

	source, err := h.lookupRepoForViewer(ctx, ownerName, name, viewer)
	if err != nil {
		writeJSON(w, http.StatusNotFound, forkCheckResult{Status: "forbidden", Message: "Source repo not found."})
		return
	}
	actor := viewer.PolicyActor()
	repoRef := policy.NewRepoRefFromRepo(source)
	if dec := policy.Can(ctx, policy.Deps{Pool: h.d.Pool}, actor, policy.ActionForkCreate, repoRef); !dec.Allow {
		writeJSON(w, http.StatusForbidden, forkCheckResult{Status: "forbidden", Message: "You can't fork this repository."})
		return
	}

	targetKind := "user"
	targetID := viewer.ID
	if tok := strings.TrimSpace(r.URL.Query().Get("target_owner")); tok != "" {
		kind, id, ok := parseOwnerToken(tok)
		if !ok {
			writeJSON(w, http.StatusBadRequest, forkCheckResult{Status: "invalid", Message: "Invalid target owner."})
			return
		}
		targetKind, targetID = kind, id
	}
	if _, ok := findForkTarget(h.ownerOptions(r), targetKind, targetID); !ok {
		writeJSON(w, http.StatusForbidden, forkCheckResult{Status: "forbidden", Message: "You can't fork into that owner."})
		return
	}

	raw := strings.TrimSpace(r.URL.Query().Get("target_name"))
	if raw == "" {
		raw = source.Name
	}
	normalized := repos.NormalizeName(raw)
	if err := repos.ValidateName(normalized); err != nil {
		writeJSON(w, http.StatusOK, forkCheckResult{Status: "invalid", Message: nameInvalidMessage(err)})
		return
	}

	// Same-owner-same-name short-circuit mirrors the orchestrator's
	// ErrSelfForkSameName guard — surface it as a taken-style hint
	// since "rename or fork elsewhere" is the user action either way.
	if (targetKind == "user" && source.OwnerUserID.Valid && source.OwnerUserID.Int64 == targetID && normalized == source.Name) ||
		(targetKind == "org" && source.OwnerOrgID.Valid && source.OwnerOrgID.Int64 == targetID && normalized == source.Name) {
		writeJSON(w, http.StatusOK, forkCheckResult{Status: "taken", Message: "Forking into the source owner requires a different name."})
		return
	}

	var exists bool
	switch targetKind {
	case "user":
		exists, err = h.rq.ExistsRepoForOwnerUser(ctx, h.d.Pool, reposdb.ExistsRepoForOwnerUserParams{
			OwnerUserID: pgtype.Int8{Int64: targetID, Valid: true},
			Name:        normalized,
		})
	case "org":
		exists, err = h.rq.ExistsRepoForOwnerOrg(ctx, h.d.Pool, reposdb.ExistsRepoForOwnerOrgParams{
			OwnerOrgID: pgtype.Int8{Int64: targetID, Valid: true},
			Name:       normalized,
		})
	}
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "fork check-name: existence query", "error", err)
		writeJSON(w, http.StatusInternalServerError, forkCheckResult{Status: "invalid", Message: "Couldn't check name availability."})
		return
	}
	if exists {
		writeJSON(w, http.StatusOK, forkCheckResult{Status: "taken", Message: "That owner already has a repository with this name."})
		return
	}
	writeJSON(w, http.StatusOK, forkCheckResult{Status: "available", Message: "Name is available."})
}

// forkCheckResult is the modal's live-validation payload shape.
type forkCheckResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// writeJSON renders body as JSON with the given status. Local to the
// repo handlers because most of them are HTML-only; the fork-name
// preview is the first JSON endpoint here. Tiny enough to inline
// rather than pull the api-package helper across a circular import.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// nameInvalidMessage flattens repos.ValidateName error variants into
// short user-facing copy. Falls back to the generic shape rule if the
// error isn't a recognized sentinel.
func nameInvalidMessage(err error) string {
	switch {
	case errors.Is(err, repos.ErrReservedName):
		return "That name is reserved."
	case errors.Is(err, repos.ErrInvalidName):
		return "Name must be lowercase letters, digits, dot, dash, or underscore (no leading dot)."
	default:
		return "Name is not valid."
	}
}

// findForkTarget locates the (kind, id) entry in the viewer's
// allowed-owner list. The matching option carries the slug needed
// for the post-create redirect URL. Forms can be hand-crafted, so
// this is the authoritative gate — `ownerOptions(r)` rebuilds the
// list per-request from the live membership state.
func findForkTarget(opts []ownerOption, kind string, id int64) (ownerOption, bool) {
	for _, o := range opts {
		if o.Kind == kind && o.ID == id {
			return o, true
		}
	}
	return ownerOption{}, false
}

// repoForkRetry handles POST /{owner}/{repo}/fork/retry. The owner
// of a fork whose init_status landed at init_failed can re-enqueue
// the clone job. The repo row stays the same — only init_status
// flips back to init_pending and the worker job restarts. Caller
// is the owner; we 403 otherwise so neutral observers can't replay
// failed clones.
func (h *Handlers) repoForkRetry(w http.ResponseWriter, r *http.Request) {
	ownerName := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	viewer := middleware.CurrentUserFromContext(r.Context())
	row, err := h.lookupRepoForViewer(r.Context(), ownerName, name, viewer)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	// Owner-only. We don't use policy.ActionRepoWrite because a
	// failed-init repo has no branches and write semantics are
	// ambiguous; "the user who minted this row can retry it" is
	// the simpler model.
	if !row.OwnerUserID.Valid || row.OwnerUserID.Int64 != viewer.ID {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	if !row.ForkOfRepoID.Valid {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "not a fork")
		return
	}
	if row.InitStatus != reposdb.RepoInitStatusInitFailed {
		// Idempotent: already pending or initialized — just redirect.
		http.Redirect(w, r, "/"+ownerName+"/"+row.Name, http.StatusSeeOther)
		return
	}
	if err := h.rq.SetRepoInitStatus(r.Context(), h.d.Pool, reposdb.SetRepoInitStatusParams{
		ID: row.ID, InitStatus: reposdb.RepoInitStatusInitPending,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "fork retry: set init_pending", "error", err, "repo_id", row.ID)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, err := worker.Enqueue(
		r.Context(), h.d.Pool, worker.KindRepoForkClone,
		map[string]any{"source_repo_id": row.ForkOfRepoID.Int64, "fork_repo_id": row.ID},
		worker.EnqueueOptions{},
	); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "fork retry: enqueue clone", "error", err, "fork_id", row.ID)
	}
	http.Redirect(w, r, "/"+ownerName+"/"+row.Name+"?notice=fork-retry-enqueued", http.StatusSeeOther)
}

// repoSync handles POST /{owner}/{repo}/sync. The repo here is the
// fork (the viewer's own copy); we authorize repo:write because
// sync mutates refs on the fork.
func (h *Handlers) repoSync(w http.ResponseWriter, r *http.Request) {
	ownerName := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	viewer := middleware.CurrentUserFromContext(r.Context())
	row, err := h.lookupRepoForViewer(r.Context(), ownerName, name, viewer)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	actor := viewer.PolicyActor()
	repoRef := policy.NewRepoRefFromRepo(row)
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoWrite, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return
	}
	if !row.ForkOfRepoID.Valid {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "not a fork")
		return
	}
	if _, err := fork.Sync(r.Context(), h.forkDeps(), viewer.ID, row.ID); err != nil {
		h.handleForkError(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+ownerName+"/"+row.Name+"?notice=fork-synced", http.StatusSeeOther)
}

// forksList renders /{owner}/{repo}/forks.
func (h *Handlers) forksList(w http.ResponseWriter, r *http.Request) {
	ownerName := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")
	row, err := h.lookupRepoForViewer(r.Context(), ownerName, name, middleware.CurrentUserFromContext(r.Context()))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	page := pageFromRequest(r)
	const pageSize = 30
	rows, err := h.rq.ListForksOfRepo(r.Context(), h.d.Pool, reposdb.ListForksOfRepoParams{
		ForkOfRepoID: pgtype.Int8{Int64: row.ID, Valid: true},
		Limit:        pageSize,
		Offset:       int32((page - 1) * pageSize),
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "list forks")
		return
	}
	total, _ := h.rq.CountForksOfRepo(r.Context(), h.d.Pool, pgtype.Int8{Int64: row.ID, Valid: true})

	// Per-row visibility filter: a private fork of a public repo
	// must only show to viewers who can see the fork. Filter via
	// policy.IsVisibleTo against the slim RepoRef shape.
	viewer := middleware.CurrentUserFromContext(r.Context())
	visibleActor := actorFor(viewer)
	deps := policy.Deps{Pool: h.d.Pool}
	visible := make([]map[string]any, 0, len(rows))
	for _, fk := range rows {
		ref := policy.RepoRef{
			ID:         fk.ID,
			Visibility: string(fk.Visibility),
		}
		// Owner not threaded through this row; for the list we just
		// gate on visibility — public visible to all, private only
		// to the fork owner (which RepoRef.OwnerUserID would catch
		// if we threaded it). The list query already excludes
		// soft-deleted rows.
		if !policy.IsVisibleTo(r.Context(), deps, visibleActor, ref) {
			continue
		}
		visible = append(visible, map[string]any{
			"OwnerUsername":    fk.OwnerUsername,
			"OwnerDisplayName": fk.OwnerDisplayName,
			"Name":             fk.Name,
			"Description":      fk.Description,
			"Visibility":       string(fk.Visibility),
			"StarCount":        fk.StarCount,
			"ForkCount":        fk.ForkCount,
			"InitStatus":       string(fk.InitStatus),
			"CreatedAt":        fk.CreatedAt.Time,
		})
	}
	common := map[string]any{
		"Title":        "Forks · " + row.Name,
		"Owner":        ownerName,
		"Repo":         row,
		"Forks":        visible,
		"Total":        total,
		"Page":         page,
		"HasNext":      int64(page*pageSize) < total,
		"HasPrev":      page > 1,
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": "forks",
	}
	if err := h.d.Render.RenderPage(w, r, "repo/forks", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "forks render", "error", err)
	}
}

// actorFor builds the policy.Actor matching a CurrentUser. Anonymous
// when the viewer is unauthenticated.
func actorFor(viewer middleware.CurrentUser) policy.Actor {
	if viewer.IsAnonymous() {
		return policy.AnonymousActor()
	}
	return viewer.PolicyActor()
}

// handleForkError maps the orchestrator's typed errors to status
// codes + friendly messages.
func (h *Handlers) handleForkError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fork.ErrNotLoggedIn):
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
	case errors.Is(err, fork.ErrSourceNotFound), errors.Is(err, fork.ErrSourceDeleted):
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
	case errors.Is(err, fork.ErrSourceArchived):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "source repo is archived")
	case errors.Is(err, fork.ErrTargetNameTaken):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "you already own a repository with that name")
	case errors.Is(err, fork.ErrSelfForkSameName):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "forking your own repo requires a different name")
	case errors.Is(err, fork.ErrVisibilityFloor):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "fork visibility cannot exceed source visibility")
	case errors.Is(err, repos.ErrDescriptionTooLong):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "description is longer than the 350-character limit")
	case errors.Is(err, fork.ErrSyncDiverged):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "fork has diverged from upstream; sync via your client")
	case errors.Is(err, fork.ErrSyncUpToDate):
		http.Redirect(w, r, r.URL.Path+"/..?notice=already-up-to-date", http.StatusSeeOther)
	case errors.Is(err, fork.ErrSyncDefaultsMissing):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "default branch missing on fork or source")
	case errors.Is(err, fork.ErrSyncRefRaced):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "fork ref changed concurrently; retry")
	case errors.Is(err, fork.ErrForkNotInitialized):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "fork is still being prepared")
	default:
		h.d.Logger.ErrorContext(r.Context(), "fork handler", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
