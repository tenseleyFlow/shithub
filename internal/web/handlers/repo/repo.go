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
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/templates"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/httpcache"
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
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	// ObjectStore serves archived Actions logs and other repo-scoped blobs.
	// nil keeps pages renderable in dev/test but archived logs show an
	// unavailable message instead of exposing storage details.
	ObjectStore storage.ObjectStore
	Audit       *audit.Recorder
	Limiter     *throttle.Limiter
	RateLimiter *ratelimit.Limiter
	CloneURLs   CloneURLs
	// SecretBox AEAD-wraps webhook secrets at rest (S33). nil disables
	// the webhook surface (the handler renders a placeholder page).
	SecretBox *secretbox.Box
	// ShithubdPath is forwarded to repos.Create so newly-init'd repos
	// have hook shims pointing at the right binary. Empty in test fixtures
	// that don't exercise hooks.
	ShithubdPath string
	// BillingEnforce carries PRO07's per-feature enforcement flags.
	// Branch-protection gating on personal private repos consults
	// these to decide whether a user-kind would-deny should block the
	// save (enforce=true) or log a report-only event and proceed
	// (PRO05 default, enforce=false).
	BillingEnforce config.EnforceConfig
	// CommitsPageCache is the F01 PR-3 in-process LRU on rendered
	// /commits/{branch} HTML. nil disables caching (tests and
	// degraded boot paths); production wires a 256-entry × 60s cache
	// from server.go. The 60s TTL is the staleness budget that
	// applies when push-side invalidation (PR-4) doesn't fire — the
	// ETag layer is process-independent and stays correct
	// regardless.
	CommitsPageCache *httpcache.PageCache
}

// Handlers is the registered handler set. Construct via New.
type Handlers struct {
	d                  Deps
	rq                 *reposdb.Queries
	uq                 *usersdb.Queries
	iq                 *issuesdb.Queries
	pq                 *pullsdb.Queries
	cq                 *checksdb.Queries
	submoduleBackfills singleflight.Group
}

// CommitsPageCache exposes the in-process LRU on rendered
// commits-list HTML (F01 PR-3). The wiring layer subscribes to
// the pagecache invalidation channel and calls
// PageCache.InvalidateBranch via this handle so the listener
// goroutine can be owned by the server lifecycle, not the
// handler builder.
func (h *Handlers) CommitsPageCache() *httpcache.PageCache {
	return h.d.CommitsPageCache
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
	if d.RateLimiter == nil {
		d.RateLimiter = ratelimit.New(d.Pool)
	}
	return &Handlers{d: d, rq: reposdb.New(), uq: usersdb.New(), iq: issuesdb.New(), pq: pullsdb.New(), cq: checksdb.New()}, nil
}

// MountNew registers /new (auth-required). Caller wraps with
// middleware.RequireUser before invoking.
func (h *Handlers) MountNew(r chi.Router) {
	r.Get("/new", h.newRepoForm)
	r.Post("/new", h.newRepoSubmit)
}

// MountRepoActionsAPI registers Actions write routes under
// /{owner}/{repo}/actions/. Caller wraps with RequireUser.
func (h *Handlers) MountRepoActionsAPI(r chi.Router) {
	r.Get("/{owner}/{repo}/actions/new", h.repoActionsNewWorkflow)
	r.Post("/{owner}/{repo}/actions/new", h.repoActionsCreateWorkflow)
	r.Post("/{owner}/{repo}/actions/workflow-pins", h.repoActionsWorkflowPin)
	r.Post("/{owner}/{repo}/actions/runs/{runIndex}/cancel", h.repoActionRunCancel)
	r.Post("/{owner}/{repo}/actions/runs/{runIndex}/rerun", h.repoActionRunRerun)
	r.Post("/{owner}/{repo}/actions/runs/{runIndex}/approve", h.repoActionRunApprove)
	r.Post("/{owner}/{repo}/actions/runs/{runIndex}/reject", h.repoActionRunReject)
	r.Post("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/cancel", h.repoActionJobCancel)
	r.Post("/{owner}/{repo}/actions/workflows/{file}/dispatches", h.repoActionsDispatch)
}

// MountRepoActionsStreams registers long-lived Actions streaming routes.
// Caller must mount this outside compression and request-timeout middleware.
func (h *Handlers) MountRepoActionsStreams(r chi.Router) {
	r.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}/log/stream", h.repoActionStepLogStream)
}

// MountRepoHome registers the root repository route plus product-tab shells
// that are intentionally public and read-gated like the Code tab. The
// two-segment route doesn't collide with the /{username} catch-all from S09;
// caller is responsible for ordering this BEFORE /{username}.
func (h *Handlers) MountRepoHome(r chi.Router) {
	r.Get("/{owner}/{repo}/actions.atom", h.repoActionsAtom)
	r.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}/log/download", h.repoActionStepLogDownload)
	r.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}", h.repoActionStepLog)
	r.Get("/{owner}/{repo}/actions/runs/{runIndex}/status", h.repoActionRunStatus)
	r.Get("/{owner}/{repo}/actions/runs/{runIndex}", h.repoActionRun)
	r.Get("/{owner}/{repo}/actions/caches", h.repoActionsCaches)
	r.Get("/{owner}/{repo}/actions/attestations", h.repoActionsAttestations)
	r.Get("/{owner}/{repo}/actions/runners", h.repoActionsRunners)
	r.Get("/{owner}/{repo}/actions/metrics/usage", h.repoActionsUsageMetrics)
	r.Get("/{owner}/{repo}/actions/metrics/performance", h.repoActionsPerformanceMetrics)
	r.Get("/{owner}/{repo}/actions/workflows/*", h.repoActionsWorkflow)
	r.Get("/{owner}/{repo}/actions", h.repoTabActions)
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Post("/{owner}/{repo}/projects", h.repoProjectCreate)
		r.Post("/{owner}/{repo}/projects/{projectID}/update", h.repoProjectUpdate)
		r.Post("/{owner}/{repo}/projects/{projectID}/state", h.repoProjectSetState)
		r.Post("/{owner}/{repo}/projects/{projectID}/delete", h.repoProjectDelete)
		r.Post("/{owner}/{repo}/packages", h.repoPackageUpload)
		r.Post("/{owner}/{repo}/packages/{packageID}/delete", h.repoPackageDelete)
		r.Get("/{owner}/{repo}/wiki/new", h.repoWikiNew)
		r.Post("/{owner}/{repo}/wiki", h.repoWikiCreate)
		r.Get("/{owner}/{repo}/wiki/{slug}/edit", h.repoWikiEdit)
		r.Post("/{owner}/{repo}/wiki/{slug}/delete", h.repoWikiDelete)
		r.Post("/{owner}/{repo}/wiki/{slug}", h.repoWikiUpdate)
	})
	r.Get("/{owner}/{repo}/projects", h.repoTabProjects)
	r.Get("/{owner}/{repo}/wiki/{slug}", h.repoWikiView)
	r.Get("/{owner}/{repo}/wiki", h.repoTabWiki)
	r.Get("/{owner}/{repo}/security", h.repoTabSecurity)
	r.Get("/{owner}/{repo}/security/advisories/new", h.repoSecurityAdvisoryNew)
	r.Post("/{owner}/{repo}/security/advisories", h.repoSecurityAdvisoryCreate)
	r.Get("/{owner}/{repo}/security/advisories/{identifier}/edit", h.repoSecurityAdvisoryEdit)
	r.Post("/{owner}/{repo}/security/advisories/{identifier}", h.repoSecurityAdvisoryUpdate)
	r.Post("/{owner}/{repo}/security/advisories/{identifier}/state", h.repoSecurityAdvisoryState)
	r.Post("/{owner}/{repo}/security/advisories/{identifier}/collaborators", h.repoSecurityAdvisoryCollaboratorAdd)
	r.Post("/{owner}/{repo}/security/advisories/{identifier}/collaborators/remove", h.repoSecurityAdvisoryCollaboratorRemove)
	r.Get("/{owner}/{repo}/security/advisories/{identifier}", h.repoSecurityAdvisoryDetail)
	r.Get("/{owner}/{repo}/security/advisories", h.repoSecurityAdvisories)
	r.Get("/{owner}/{repo}/security/code-scanning", h.repoCodeScanning)
	r.Post("/{owner}/{repo}/security/code-scanning/upload", h.repoCodeScanningUpload)
	r.Post("/{owner}/{repo}/security/code-scanning/campaigns", h.repoCodeSecurityCampaignCreate)
	r.Post("/{owner}/{repo}/security/code-scanning/campaigns/{campaignID}/state", h.repoCodeSecurityCampaignState)
	r.Get("/{owner}/{repo}/security/secret-scanning", h.repoSecretScanning)
	r.Post("/{owner}/{repo}/security/secret-scanning/scan", h.repoSecretScanningRunScan)
	r.Post("/{owner}/{repo}/security/secret-scanning/allowlist", h.repoSecretScanningAllowlistAdd)
	r.Post("/{owner}/{repo}/security/secret-scanning/allowlist/{id}/remove", h.repoSecretScanningAllowlistRemove)
	r.Post("/{owner}/{repo}/security/secret-scanning/bypass/{id}/approve", h.repoSecretScanningBypassApprove)
	r.Post("/{owner}/{repo}/security/secret-scanning/bypass/{id}/deny", h.repoSecretScanningBypassDeny)
	r.Get("/{owner}/{repo}/pulse", h.repoInsightsPulse)
	r.Get("/{owner}/{repo}/graphs/contributors", h.repoInsightsContributors)
	r.Get("/{owner}/{repo}/graphs/commit-activity", h.repoInsightsCommitActivity)
	r.Get("/{owner}/{repo}/graphs/code-frequency", h.repoInsightsCodeFrequency)
	r.Get("/{owner}/{repo}/graphs/traffic", h.repoInsightsTraffic)
	r.Get("/{owner}/{repo}/network", h.repoInsightsNetwork)
	r.Get("/{owner}/{repo}/packages/files/{fileID}/download", h.repoPackageDownload)
	r.Get("/{owner}/{repo}/packages", h.repoTabPackages)
	r.Get("/{owner}/{repo}/releases", h.repoTabReleases)
	r.Get("/{owner}/{repo}", h.repoHome)
}

// newRepoForm renders GET /new. When ?template_repo_id=N is present
// (the "Use this template" button hands off through this query
// param), the renderer loads the template's owner/name for a banner
// + a hidden template_repo_id input that the submit handler reads to
// route into newRepoFromTemplate. Invalid / unreadable template ids
// are silently dropped to existence-leak-safe — the form renders
// without the banner and the user falls back to a normal create.
func (h *Handlers) newRepoForm(w http.ResponseWriter, r *http.Request) {
	form := formState{
		Owner:      strings.TrimSpace(r.URL.Query().Get("owner")),
		Visibility: "public",
	}
	if rawID := strings.TrimSpace(r.URL.Query().Get("template_repo_id")); rawID != "" {
		if id, err := strconv.ParseInt(rawID, 10, 64); err == nil && id > 0 {
			if tpl, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, id); err == nil && tpl.IsTemplate {
				viewer := middleware.CurrentUserFromContext(r.Context())
				actor := viewer.PolicyActor()
				if policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoRead, policy.NewRepoRefFromRepo(tpl)).Allow {
					form.TemplateRepoID = id
					form.Visibility = string(tpl.Visibility)
				}
			}
		}
	}
	h.renderNewForm(w, r, form, "")
}

// formState mirrors the new-repo form so a re-render after a validation
// error can repopulate the user's input.
type formState struct {
	Owner          string // "user:<id>" or "org:<id>"
	Name           string
	Description    string
	Visibility     string
	SourceRemote   string
	InitReadme     bool
	License        string
	Gitignore      string
	TemplateRepoID int64 // PRO-EXT01-06pre-b: non-zero routes the submit through newRepoFromTemplate
}

// ownerOption is one entry the new-repo owner picker shows.
type ownerOption struct {
	Kind    string // "user" | "org"
	ID      int64
	Slug    string
	Display string
	// Token is the form value: "user:<id>" or "org:<id>".
	Token string
}

// newRepoSubmit handles POST /new.
func (h *Handlers) newRepoSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	// PRO-EXT01-06pre-b: if template_repo_id is present, the create
	// takes the create-from-template branch entirely (different
	// orchestrator, different worker job, no License/Readme init
	// applied — the template's tree is what initializes the repo).
	if rawTemplateID := strings.TrimSpace(r.PostFormValue("template_repo_id")); rawTemplateID != "" {
		h.newRepoFromTemplate(w, r, user, rawTemplateID)
		return
	}
	form := formState{
		Owner:        strings.TrimSpace(r.PostFormValue("owner")),
		Name:         repos.NormalizeName(r.PostFormValue("name")),
		Description:  strings.TrimSpace(r.PostFormValue("description")),
		Visibility:   strings.TrimSpace(r.PostFormValue("visibility")),
		SourceRemote: strings.TrimSpace(r.PostFormValue("source_remote_url")),
		InitReadme:   r.PostFormValue("init_readme") == "on",
		License:      strings.TrimSpace(r.PostFormValue("license")),
		Gitignore:    strings.TrimSpace(r.PostFormValue("gitignore")),
	}
	if form.Visibility == "" {
		form.Visibility = "public"
	}
	var sourceRemoteURL string
	if form.SourceRemote != "" {
		if form.InitReadme || form.License != "" || form.Gitignore != "" {
			h.renderNewForm(w, r, form, "Source imports can't be combined with initial README, license, or .gitignore files.")
			return
		}
		var sourceErr error
		sourceRemoteURL, sourceErr = repos.ValidateSourceRemoteURL(r.Context(), form.SourceRemote)
		if sourceErr != nil {
			h.renderNewForm(w, r, form, "Source remote URL must be a public http(s) git remote without credentials.")
			return
		}
		form.SourceRemote = sourceRemoteURL
	}

	params := repos.Params{
		ActorUserID:      user.ID,
		ActorIsSiteAdmin: user.IsSiteAdmin,
		Name:             form.Name,
		Description:      form.Description,
		Visibility:       form.Visibility,
		InitReadme:       form.InitReadme,
		LicenseKey:       form.License,
		GitignoreKey:     form.Gitignore,
	}
	// Owner picker: "org:N" routes through the org-owner branch with
	// the per-org allow_member_repo_create gate; anything else
	// defaults to the viewer's personal namespace.
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
		params.OwnerOrgID = org.ID
		params.OwnerSlug = string(org.Slug)
	} else {
		params.OwnerUserID = user.ID
		params.OwnerUsername = user.Username
	}

	res, err := repos.Create(r.Context(), repos.Deps{
		Pool:         h.d.Pool,
		RepoFS:       h.d.RepoFS,
		Audit:        h.d.Audit,
		Limiter:      h.d.Limiter,
		Logger:       h.d.Logger,
		ShithubdPath: h.d.ShithubdPath,
	}, params)
	if err != nil {
		// Surface the real error in the journal — friendlyCreateError
		// collapses every non-typed cause into the bland "Could not
		// create the repository" string the user sees, so without
		// this log line the operator has no signal to triage a failed
		// create.
		h.d.Logger.WarnContext(
			r.Context(), "repos: create failed",
			"error", err,
			"actor_user_id", params.ActorUserID,
			"owner_user_id", params.OwnerUserID,
			"owner_org_id", params.OwnerOrgID,
			"name", params.Name,
			"visibility", params.Visibility,
		)
		h.renderNewForm(w, r, form, friendlyCreateError(err))
		return
	}
	ownerSlug := params.OwnerUsername
	if ownerSlug == "" {
		ownerSlug = params.OwnerSlug
	}
	if sourceRemoteURL != "" {
		if _, err := h.rq.UpsertRepoSourceRemote(r.Context(), h.d.Pool, reposdb.UpsertRepoSourceRemoteParams{
			RepoID:    res.Repo.ID,
			RemoteUrl: sourceRemoteURL,
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "repos: source remote save after create", "error", err, "repo_id", res.Repo.ID)
			http.Redirect(w, r, "/"+ownerSlug+"/"+res.Repo.Name+"/settings/general?notice=source-remote-save-failed", http.StatusSeeOther)
			return
		}
		if err := h.fetchRepoSourceRemote(r.Context(), res.Repo, ownerSlug, sourceRemoteURL); err != nil {
			h.d.Logger.WarnContext(r.Context(), "repos: source remote fetch after create", "error", err, "repo_id", res.Repo.ID, "remote", sourceRemoteURL)
			http.Redirect(w, r, "/"+ownerSlug+"/"+res.Repo.Name+"/settings/general?notice=source-remote-fetch-failed", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/"+ownerSlug+"/"+res.Repo.Name, http.StatusSeeOther)
}

// parseOwnerToken splits a value like "org:42" into ("org", 42, true).
// Returns ok=false on missing or unparseable input.
func parseOwnerToken(s string) (kind string, id int64, ok bool) {
	if s == "" {
		return "", 0, false
	}
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return "", 0, false
	}
	kind = s[:colon]
	if kind != "user" && kind != "org" {
		return "", 0, false
	}
	n, err := strconv.ParseInt(s[colon+1:], 10, 64)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return kind, n, true
}

func (h *Handlers) renderNewForm(w http.ResponseWriter, r *http.Request, form formState, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	owners := h.ownerOptions(r)
	// Default the form's owner pick to the viewer when the field is
	// empty or invalid. GET /new?owner=<slug> can preselect an org,
	// but only after matching that slug against the viewer's allowed
	// owner choices.
	form.Owner = selectedOwnerToken(form.Owner, owners)
	if err := h.d.Render.RenderPage(w, r, "repo/new", map[string]any{
		"Title":      "New repository",
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Form":       form,
		"Error":      errMsg,
		"Licenses":   templates.Licenses(),
		"Gitignores": templates.Gitignores(),
		"Owners":     owners,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render new", "error", err)
	}
}

func selectedOwnerToken(raw string, owners []ownerOption) string {
	if len(owners) == 0 {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if raw != "" {
		for _, owner := range owners {
			if raw == owner.Token {
				return owner.Token
			}
		}
		for _, owner := range owners {
			if strings.EqualFold(raw, owner.Slug) {
				return owner.Token
			}
		}
	}
	return owners[0].Token
}

// ownerOptions returns the entries the form's owner picker shows:
// the viewer themselves plus every org they're a member of where
// they're allowed to create (owner role OR allow_member_repo_create).
func (h *Handlers) ownerOptions(r *http.Request) []ownerOption {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.IsAnonymous() {
		return nil
	}
	out := []ownerOption{{
		Kind: "user", ID: user.ID, Slug: user.Username,
		Display: user.Username,
		Token:   "user:" + strconv.FormatInt(user.ID, 10),
	}}
	memberships, err := orgsdb.New().ListOrgsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		return out
	}
	for _, m := range memberships {
		isOwner := m.Role == orgsdb.OrgRoleOwner
		canCreate := isOwner
		if !isOwner {
			full, ferr := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, m.OrgID)
			if ferr == nil {
				canCreate = full.AllowMemberRepoCreate
			}
		}
		if !canCreate {
			continue
		}
		display := m.DisplayName
		if display == "" {
			display = string(m.Slug)
		}
		out = append(out, ownerOption{
			Kind:    "org",
			ID:      m.OrgID,
			Slug:    string(m.Slug),
			Display: display,
			Token:   "org:" + strconv.FormatInt(m.OrgID, 10),
		})
	}
	return out
}

// repoHome serves GET /{owner}/{repo}. Empty repos render the quick setup
// placeholder; populated repos render the Code tab at the repository home URL.
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

	// Keep the populated repository home at /owner/name, matching GitHub's
	// Code tab instead of forcing users through the /tree/default-branch URL.
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
		refs, refsErr := repogit.ListRefs(r.Context(), diskPath)
		if refsErr != nil {
			h.d.Logger.WarnContext(r.Context(), "repo: ListRefs", "error", refsErr)
		}
		h.renderRepoTree(w, r, &codeContext{
			owner:   owner,
			row:     row,
			gitDir:  diskPath,
			refs:    refs,
			allRefs: refNames(refs),
			ref:     row.DefaultBranch,
			subpath: "",
		})
		return
	}
	h.recordRepoView(r, row, owner)

	common := map[string]any{
		"Title":         row.Name + " · " + owner,
		"CSRFToken":     middleware.CSRFTokenForRequest(r),
		"Owner":         owner,
		"Repo":          row,
		"DefaultBranch": row.DefaultBranch,
		"HTTPSCloneURL": h.cloneHTTPS(owner, row.Name),
		"SSHEnabled":    h.d.CloneURLs.SSHEnabled,
		"SSHCloneURL":   h.cloneSSH(owner, row.Name),
		"RepoActions":   h.repoActions(r, row.ID),
		"RepoCounts":    h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":   h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav":  "code",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Branch on init_status: failed and in-progress shells render
	// purpose-built pages instead of the empty-repo bootstrap.
	// Pre-fix-B this was conflated — failed-init shells looked
	// identical to fresh-but-empty repos, which surfaced as "your
	// fork worked, here's how to clone" misdirection.
	switch row.InitStatus {
	case reposdb.RepoInitStatusInitFailed:
		h.renderRepoInitFailed(w, r, owner, row, common)
		return
	case reposdb.RepoInitStatusInitPending:
		h.renderRepoInitPending(w, r, row, common)
		return
	}
	// Empty path only — populated repos already redirected above.
	if err := h.d.Render.RenderPage(w, r, "repo/empty", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render empty", "error", err)
	}
}

// renderRepoInitPending serves a "cloning, refresh in 5s" page for
// rows whose worker job hasn't finished yet. Shared between
// freshly-clicked forks and retries; both flip init_status to
// init_pending before enqueuing.
func (h *Handlers) renderRepoInitPending(w http.ResponseWriter, r *http.Request, row reposdb.Repo, common map[string]any) {
	isFork := row.ForkOfRepoID.Valid
	var (
		sourceFullName string
		sourceLink     string
	)
	if isFork {
		src, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, row.ForkOfRepoID.Int64)
		if err == nil && !src.DeletedAt.Valid {
			var srcOwner string
			switch {
			case src.OwnerUserID.Valid:
				if u, uerr := usersdb.New().GetUserByID(r.Context(), h.d.Pool, src.OwnerUserID.Int64); uerr == nil {
					srcOwner = u.Username
				}
			case src.OwnerOrgID.Valid:
				if o, oerr := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, src.OwnerOrgID.Int64); oerr == nil {
					srcOwner = o.Slug
				}
			}
			if srcOwner != "" {
				sourceFullName = srcOwner + "/" + src.Name
				sourceLink = "/" + srcOwner + "/" + src.Name
			} else {
				sourceFullName = "the upstream repository"
			}
		} else {
			sourceFullName = "the upstream repository"
		}
	}
	common["IsFork"] = isFork
	common["SourceFullName"] = sourceFullName
	common["SourceLink"] = sourceLink
	if err := h.d.Render.RenderPage(w, r, "repo/init_pending", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render init_pending", "error", err)
	}
}

// renderRepoInitFailed serves the "fork failed / repo init failed"
// page. Forks get a retry affordance and a delete shortcut; non-fork
// init_failed rows get just delete (there's no generic retry — only
// the fork_clone job knows how to drive its retry).
func (h *Handlers) renderRepoInitFailed(w http.ResponseWriter, r *http.Request, owner string, row reposdb.Repo, common map[string]any) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	isFork := row.ForkOfRepoID.Valid
	var (
		sourceFullName string
		sourceLink     string
	)
	if isFork {
		src, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, row.ForkOfRepoID.Int64)
		if err == nil && !src.DeletedAt.Valid {
			var srcOwner string
			switch {
			case src.OwnerUserID.Valid:
				if u, uerr := usersdb.New().GetUserByID(r.Context(), h.d.Pool, src.OwnerUserID.Int64); uerr == nil {
					srcOwner = u.Username
				}
			case src.OwnerOrgID.Valid:
				if o, oerr := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, src.OwnerOrgID.Int64); oerr == nil {
					srcOwner = o.Slug
				}
			}
			if srcOwner != "" {
				sourceFullName = srcOwner + "/" + src.Name
				sourceLink = "/" + srcOwner + "/" + src.Name
			} else {
				sourceFullName = "the upstream repository"
			}
		} else {
			sourceFullName = "the upstream repository"
		}
	}
	common["IsFork"] = isFork
	common["SourceFullName"] = sourceFullName
	common["SourceLink"] = sourceLink
	common["CanManage"] = !viewer.IsAnonymous() && row.OwnerUserID.Valid && row.OwnerUserID.Int64 == viewer.ID
	common["RetryAction"] = "/" + owner + "/" + row.Name + "/fork/retry"
	common["SettingsLink"] = "/" + owner + "/" + row.Name + "/settings"
	if err := h.d.Render.RenderPage(w, r, "repo/init_failed", common); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo: render init_failed", "error", err)
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
		actor = viewer.PolicyActor()
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
	case errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded):
		return err.Error()
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
