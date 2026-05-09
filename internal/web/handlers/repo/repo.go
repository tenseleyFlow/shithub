// SPDX-License-Identifier: AGPL-3.0-or-later

// Package repo wires the HTTP handlers for repository creation and the
// (placeholder) repo home page. The actual orchestration lives in
// internal/repos; this package is the thin web layer that calls into it.
package repo

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/templates"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// CloneURLs configures the operator-visible HTTPS / SSH endpoints we
// surface on the empty-repo placeholder. SSHEnabled flips whether the
// "Clone over SSH" snippet renders at all (S12 wires the protocol).
type CloneURLs struct {
	BaseURL    string // e.g. "https://shithub.example"
	SSHEnabled bool
	SSHHost    string // e.g. "git@shithub.example" — only used when SSHEnabled
}

// cloneHTTPS / cloneSSH compose the per-repo URL strings that every
// "Code" view drops into the clone dropdown. Centralized so the
// per-template plumbing in code views and the repo home stay
// consistent (and switching to a `git://` later is a one-line edit).
func (h *Handlers) cloneHTTPS(owner, name string) string {
	return h.d.CloneURLs.BaseURL + "/" + owner + "/" + name + ".git"
}
func (h *Handlers) cloneSSH(owner, name string) string {
	return h.d.CloneURLs.SSHHost + ":" + owner + "/" + name + ".git"
}

// Deps wires the handler set.
type Deps struct {
	Logger    *slog.Logger
	Render    *render.Renderer
	Pool      *pgxpool.Pool
	RepoFS    *storage.RepoFS
	Audit     *audit.Recorder
	Limiter   *throttle.Limiter
	CloneURLs CloneURLs
	// ShithubdPath is forwarded to repos.Create so newly-init'd repos
	// have hook shims pointing at the right binary. Empty in test fixtures
	// that don't exercise hooks.
	ShithubdPath string
}

// Handlers is the registered handler set. Construct via New.
type Handlers struct {
	d  Deps
	rq *reposdb.Queries
	uq *usersdb.Queries
	iq *issuesdb.Queries
	pq *pullsdb.Queries
	cq *checksdb.Queries
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("repo: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("repo: nil Pool")
	}
	if d.RepoFS == nil {
		return nil, errors.New("repo: nil RepoFS")
	}
	if d.Audit == nil {
		d.Audit = audit.NewRecorder()
	}
	if d.Limiter == nil {
		d.Limiter = throttle.NewLimiter()
	}
	return &Handlers{d: d, rq: reposdb.New(), uq: usersdb.New(), iq: issuesdb.New(), pq: pullsdb.New(), cq: checksdb.New()}, nil
}

// MountNew registers /new (auth-required). Caller wraps with
// middleware.RequireUser before invoking.
func (h *Handlers) MountNew(r chi.Router) {
	r.Get("/new", h.newRepoForm)
	r.Post("/new", h.newRepoSubmit)
}

// MountRepoHome registers /{owner}/{repo}. This is a 2-segment route so
// it doesn't collide with the /{username} catch-all from S09. Caller is
// responsible for ordering: register this BEFORE /{username}.
func (h *Handlers) MountRepoHome(r chi.Router) {
	r.Get("/{owner}/{repo}", h.repoHome)
}

// newRepoForm renders GET /new.
func (h *Handlers) newRepoForm(w http.ResponseWriter, r *http.Request) {
	h.renderNewForm(w, r, formState{
		Visibility: "public",
	}, "")
}

// formState mirrors the new-repo form so a re-render after a validation
// error can repopulate the user's input.
type formState struct {
	Name        string
	Description string
	Visibility  string
	InitReadme  bool
	License     string
	Gitignore   string
}

// newRepoSubmit handles POST /new.
func (h *Handlers) newRepoSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	form := formState{
		Name:        repos.NormalizeName(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Visibility:  strings.TrimSpace(r.PostFormValue("visibility")),
		InitReadme:  r.PostFormValue("init_readme") == "on",
		License:     strings.TrimSpace(r.PostFormValue("license")),
		Gitignore:   strings.TrimSpace(r.PostFormValue("gitignore")),
	}
	if form.Visibility == "" {
		form.Visibility = "public"
	}

	res, err := repos.Create(r.Context(), repos.Deps{
		Pool:         h.d.Pool,
		RepoFS:       h.d.RepoFS,
		Audit:        h.d.Audit,
		Limiter:      h.d.Limiter,
		Logger:       h.d.Logger,
		ShithubdPath: h.d.ShithubdPath,
	}, repos.Params{
		OwnerUserID:   user.ID,
		OwnerUsername: user.Username,
		Name:          form.Name,
		Description:   form.Description,
		Visibility:    form.Visibility,
		InitReadme:    form.InitReadme,
		LicenseKey:    form.License,
		GitignoreKey:  form.Gitignore,
	})
	if err != nil {
		h.renderNewForm(w, r, form, friendlyCreateError(err))
		return
	}

	http.Redirect(w, r, "/"+user.Username+"/"+res.Repo.Name, http.StatusSeeOther)
}

func (h *Handlers) renderNewForm(w http.ResponseWriter, r *http.Request, form formState, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "repo/new", map[string]any{
		"Title":      "New repository",
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Form":       form,
		"Error":      errMsg,
		"Licenses":   templates.Licenses(),
		"Gitignores": templates.Gitignores(),
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render new", "error", err)
	}
}

// repoHome serves GET /{owner}/{repo}. Forks on whether the bare repo
// has any branches: empty → quick-setup placeholder; populated → a slim
// "post-push" view with the head commit on the default branch. The full
// tree/file listing is S17.
func (h *Handlers) repoHome(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "repo")

	row, err := h.lookupRepoForViewer(r.Context(), owner, name, middleware.CurrentUserFromContext(r.Context()))
	if err != nil {
		// Maybe the (owner, name) is a stale name; look up the redirect
		// table and 301 to the canonical URL so old bookmarks keep
		// working. Authoritative miss after the redirect check 404s.
		if newURL := h.tryRedirect(r, owner, name); newURL != "" {
			http.Redirect(w, r, newURL, http.StatusMovedPermanently)
			return
		}
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}

	// S17: when the repo has any branch, the canonical view is the
	// tree at default_branch. The S11 quick-setup placeholder still
	// covers the empty case.
	diskPath, fsErr := h.d.RepoFS.RepoPath(owner, row.Name)
	hasBranch := false
	if fsErr == nil {
		if ok, herr := repogit.HasAnyBranch(r.Context(), diskPath); herr == nil {
			hasBranch = ok
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo: HasAnyBranch", "error", herr)
		}
	}
	if hasBranch {
		http.Redirect(w, r, "/"+owner+"/"+row.Name+"/tree/"+row.DefaultBranch, http.StatusSeeOther)
		return
	}

	common := map[string]any{
		"Title":         row.Name + " · " + owner,
		"CSRFToken":     middleware.CSRFTokenForRequest(r),
		"Owner":         owner,
		"Repo":          row,
		"DefaultBranch": row.DefaultBranch,
		"HTTPSCloneURL": h.cloneHTTPS(owner, row.Name),
		"SSHEnabled":    h.d.CloneURLs.SSHEnabled,
		"SSHCloneURL":   h.cloneSSH(owner, row.Name),
		"RepoCounts":    h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":   h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav":  "code",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Empty path only — populated repos already redirected above.
	if err := h.d.Render.RenderPage(w, r, "repo/empty", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render empty", "error", err)
	}
}

// lookupRepoForViewer returns the repo row when:
//   - it exists,
//   - it is not soft-deleted,
//   - AND the viewer is allowed to see it (public OR viewer is owner).
//
// Anything else returns ErrNoRows so the caller can 404 uniformly.
//
// Owner kind is dispatched via principals: ownerName resolves to
// either a user_id or an org_id via the same single-source-of-truth
// table that drives /{slug} routing. Both kinds resolve through the
// same indexed lookup so the cost is identical.
func (h *Handlers) lookupRepoForViewer(ctx context.Context, ownerName, repoName string, viewer middleware.CurrentUser) (reposdb.Repo, error) {
	principal, err := orgs.Resolve(ctx, h.d.Pool, ownerName)
	if err != nil {
		return reposdb.Repo{}, pgx.ErrNoRows
	}
	var row reposdb.Repo
	switch principal.Kind {
	case orgs.PrincipalUser:
		row, err = h.rq.GetRepoByOwnerUserAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:        repoName,
		})
	case orgs.PrincipalOrg:
		row, err = h.rq.GetRepoByOwnerOrgAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:       repoName,
		})
	default:
		return reposdb.Repo{}, pgx.ErrNoRows
	}
	if err != nil {
		return reposdb.Repo{}, err
	}
	// Visibility decision delegated to policy.Can. We use ActionRepoRead;
	// when the decision denies, return ErrNoRows so the caller can 404.
	repoRef := policy.NewRepoRefFromRepo(row)
	var actor policy.Actor
	if viewer.IsAnonymous() {
		actor = policy.AnonymousActor()
	} else {
		actor = policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	}
	// ActionRepoRead deny on a private repo with a non-collab viewer is
	// indistinguishable from "doesn't exist" — Maybe404 returns 404 in
	// that shape, so the caller's pgx.ErrNoRows fallthrough is the
	// right shape regardless of whether the row was missing or just
	// invisible. Honest 403s (e.g. ActionRepoWrite on archived) are
	// gated through `loadRepoAndAuthorize`, not this read-only helper.
	if !policy.Can(ctx, policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoRead, repoRef).Allow {
		return reposdb.Repo{}, pgx.ErrNoRows
	}
	return row, nil
}

// friendlyCreateError maps the typed errors from internal/repos to the
// strings the form surfaces to the user.
func friendlyCreateError(err error) string {
	switch {
	case errors.Is(err, repos.ErrInvalidName):
		return "Name must be 1–100 chars: lowercase letters, digits, dots, hyphens, underscores."
	case errors.Is(err, repos.ErrReservedName):
		return "That name is reserved."
	case errors.Is(err, repos.ErrTaken):
		return "You already own a repository with that name."
	case errors.Is(err, repos.ErrNoVerifiedEmail):
		return "Verify a primary email before creating a repository — we use it for the initial commit author."
	case errors.Is(err, repos.ErrDescriptionTooLong):
		return "Description is too long (max 350 characters)."
	case errors.Is(err, repos.ErrUnknownLicense):
		return "Unknown license selection."
	case errors.Is(err, repos.ErrUnknownGitignore):
		return "Unknown .gitignore selection."
	}
	if t, ok := isThrottled(err); ok {
		return "You're creating repositories too quickly. Try again in " + t + "."
	}
	return "Could not create the repository. Try again."
}

// isThrottled extracts the user-friendly retry-after string from the
// throttle package's typed error, if that's what we caught.
func isThrottled(err error) (string, bool) {
	var t *throttle.ErrThrottled
	if errors.As(err, &t) {
		return t.RetryAfter.String(), true
	}
	return "", false
}
