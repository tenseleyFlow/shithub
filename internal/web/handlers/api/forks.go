// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/fork"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// mountForks registers the S50 §13 forks REST surface.
//
//	GET  /api/v1/repos/{o}/{r}/forks[?page=&per_page=]
//	POST /api/v1/repos/{o}/{r}/forks
//
// GET lists forks of the repo (paginated). POST creates a fork into
// the authenticated user's namespace; the on-disk clone runs in the
// background worker (`repo:fork_clone`), so the fork row comes back
// with `init_status: "init_pending"` and the URL resolves
// immediately.
//
// Scopes: `repo:read` on GET, `repo:write` on POST. Policy gates are
// `ActionRepoRead` and `ActionForkCreate` respectively.
func (h *Handlers) mountForks(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/forks", h.forksList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/forks", h.forkCreate)
	})
}

type forkResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	OwnerLogin    string `json:"owner_login"`
	OwnerName     string `json:"owner_display_name,omitempty"`
	Description   string `json:"description,omitempty"`
	Visibility    string `json:"visibility"`
	StarCount     int64  `json:"star_count"`
	ForkCount     int64  `json:"fork_count"`
	InitStatus    string `json:"init_status"`
	CreatedAt     string `json:"created_at"`
	SourceRepoID  int64  `json:"source_repo_id,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

func (h *Handlers) forksList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	rq := reposdb.New()
	total, err := rq.CountForksOfRepo(r.Context(), h.d.Pool, pgtype.Int8{Int64: repo.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count forks", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := rq.ListForksOfRepo(r.Context(), h.d.Pool, reposdb.ListForksOfRepoParams{
		ForkOfRepoID: pgtype.Int8{Int64: repo.ID, Valid: true},
		Limit:        int32(perPage),
		Offset:       int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list forks", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	// Per-row visibility filter: private forks of a public source
	// must only surface to viewers who can see them. Same approach
	// the HTML handler uses.
	auth := middleware.PATAuthFromContext(r.Context())
	deps := policy.Deps{Pool: h.d.Pool}
	actor := auth.PolicyActor()
	out := make([]forkResponse, 0, len(rows))
	for _, fk := range rows {
		ref := policy.RepoRef{ID: fk.ID, Visibility: string(fk.Visibility)}
		if !policy.IsVisibleTo(r.Context(), deps, actor, ref) {
			continue
		}
		out = append(out, forkResponse{
			ID:          fk.ID,
			Name:        fk.Name,
			OwnerLogin:  fk.OwnerUsername,
			OwnerName:   fk.OwnerDisplayName,
			Description: fk.Description,
			Visibility:  string(fk.Visibility),
			StarCount:   fk.StarCount,
			ForkCount:   fk.ForkCount,
			InitStatus:  string(fk.InitStatus),
			CreatedAt:   fk.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

type forkCreateRequest struct {
	// Name lets the caller fork into a different repo name under
	// their own account. Empty defaults to the source name.
	Name string `json:"name"`
	// DefaultBranch is accepted for forward compatibility but is
	// ignored today; the fork inherits the source's default branch.
	DefaultBranch string `json:"default_branch"`
	// Visibility lets the caller fork a public source to a private
	// fork. Empty defaults to the source visibility (private sources
	// are pinned to private).
	Visibility string `json:"visibility"`
	// Description lets the caller override the fork's description
	// at create time. Empty (after trim) defaults to the source's
	// description. Validated server-side against the ≤350-char DB
	// CHECK.
	Description string `json:"description"`
}

func (h *Handlers) forkCreate(w http.ResponseWriter, r *http.Request) {
	source, ok := h.resolveAPIRepo(w, r, policy.ActionForkCreate)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body forkCreateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	res, err := fork.Create(r.Context(), fork.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Audit:  h.d.Audit,
		Logger: h.d.Logger,
	}, fork.CreateParams{
		SourceRepoID:      source.ID,
		ActorUserID:       auth.UserID,
		TargetOwnerID:     auth.UserID, // self-fork; org targets need a separate route
		TargetName:        strings.TrimSpace(body.Name),
		TargetVisibility:  strings.TrimSpace(body.Visibility),
		TargetDescription: body.Description,
	})
	if err != nil {
		writeForkError(w, err)
		return
	}
	// Enqueue the on-disk clone; identical pattern to the HTML flow.
	// Failure to enqueue is logged but doesn't unwind the row create:
	// the fork URL resolves with init_status=init_pending and the
	// admin can re-enqueue if needed.
	if _, qErr := worker.Enqueue(
		r.Context(), h.d.Pool, worker.KindRepoForkClone,
		map[string]any{"source_repo_id": res.Source.ID, "fork_repo_id": res.Fork.ID},
		worker.EnqueueOptions{},
	); qErr != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: enqueue fork clone", "error", qErr, "fork_id", res.Fork.ID)
	}
	// Owner login is the actor (self-fork) — pull it from the auth context.
	user, _ := h.q.GetUserByID(r.Context(), h.d.Pool, auth.UserID)
	writeJSON(w, http.StatusCreated, forkResponse{
		ID:            res.Fork.ID,
		Name:          res.Fork.Name,
		OwnerLogin:    user.Username,
		OwnerName:     user.DisplayName,
		Description:   res.Fork.Description,
		Visibility:    string(res.Fork.Visibility),
		StarCount:     res.Fork.StarCount,
		ForkCount:     res.Fork.ForkCount,
		InitStatus:    string(res.Fork.InitStatus),
		CreatedAt:     res.Fork.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		SourceRepoID:  res.Source.ID,
		DefaultBranch: res.Fork.DefaultBranch,
	})
}

func writeForkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fork.ErrNotLoggedIn):
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, fork.ErrSourceNotFound), errors.Is(err, fork.ErrSourceDeleted):
		writeAPIError(w, http.StatusNotFound, "source repo not found")
	case errors.Is(err, fork.ErrSourceArchived):
		writeAPIError(w, http.StatusConflict, "source repo is archived")
	case errors.Is(err, fork.ErrSourcePaused):
		writeAPIError(w, http.StatusConflict, "source repo is paused")
	case errors.Is(err, fork.ErrTargetNameTaken):
		writeAPIError(w, http.StatusConflict, "you already own a repository with that name")
	case errors.Is(err, fork.ErrSelfForkSameName):
		writeAPIError(w, http.StatusConflict, "forking your own repo requires a different name")
	case errors.Is(err, fork.ErrVisibilityFloor):
		writeAPIError(w, http.StatusUnprocessableEntity, "fork visibility cannot exceed source visibility")
	case errors.Is(err, repos.ErrDescriptionTooLong):
		writeAPIError(w, http.StatusUnprocessableEntity, "description is longer than the 350-character limit")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
