// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/templates"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountRepos registers the S50 §2 REST surface for repositories.
//
//	GET    /api/v1/user/repos                 list authenticated user's repos
//	GET    /api/v1/users/{username}/repos     list a user's public repos
//	GET    /api/v1/orgs/{org}/repos           list an org's repos (visibility-aware)
//	GET    /api/v1/repos/{owner}/{repo}       fetch a single repo
//	POST   /api/v1/user/repos                 create personal repo
//	POST   /api/v1/orgs/{org}/repos           create org-owned repo
//	PATCH  /api/v1/repos/{owner}/{repo}       update mutable repo settings
//	DELETE /api/v1/repos/{owner}/{repo}       soft-delete a repo
//
// Scopes: repo:read for GETs, repo:write for POST/PATCH/DELETE. Existence
// leaks are smothered behind policy gates that 404 instead of 403 when
// the caller can't see the resource.
func (h *Handlers) mountRepos(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/user/repos", h.userReposList)
		r.Get("/api/v1/users/{username}/repos", h.userPublicReposList)
		r.Get("/api/v1/orgs/{org}/repos", h.orgReposList)
		r.Get("/api/v1/repos/{owner}/{repo}", h.repoGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/user/repos", h.userRepoCreate)
		r.Post("/api/v1/orgs/{org}/repos", h.orgRepoCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}", h.repoPatch)
		r.Delete("/api/v1/repos/{owner}/{repo}", h.repoDelete)
	})
}

// repoOwnerEnvelope is the nested owner shape gh-compat clients
// (shithub-cli's repos.Repo, gh CLI's Repository) expect. Lives
// alongside the legacy flat OwnerLogin / OwnerType fields during
// the S62 audit migration (one release cycle, same as S60 users).
type repoOwnerEnvelope struct {
	Login string `json:"login"`
	Type  string `json:"type"`
	ID    int64  `json:"id,omitempty"`
}

// repoLicenseEnvelope is the gh-compat license shape. Key is the SPDX
// id lowercased (e.g. "mit"); SPDXID carries the canonical SPDX casing
// (e.g. "MIT"); Name is the human-readable title (e.g. "MIT License").
// URL points back at the /licenses discovery endpoint added in I35.
// NodeID is the opaque base64 of `gid://shithub/License/{key}` —
// gh-compat clients use it as the cache key.
//
// G13 (F8): pre-fix Name was always empty. I7a (audit-I11) adds the
// SPDX casing, URL, and NodeID so the envelope matches gh's shape
// instead of carrying just the two key+name fields.
type repoLicenseEnvelope struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	SPDXID string `json:"spdx_id,omitempty"`
	URL    string `json:"url,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// repoSecurityAndAnalysis is the gh-compat security envelope. shithub
// doesn't ship secret-scanning or Dependabot today, but gh-compat
// clients (Dependabot itself, Renovate, security CI tooling) probe
// `security_and_analysis.*.status` to feature-detect. Emitting the
// shape with `disabled` lets those clients skip cleanly instead of
// crashing on a missing field.
type repoSecurityAndAnalysis struct {
	SecretScanning               repoSecurityFeature `json:"secret_scanning"`
	SecretScanningPushProtection repoSecurityFeature `json:"secret_scanning_push_protection"`
	DependabotSecurityUpdates    repoSecurityFeature `json:"dependabot_security_updates"`
}

// repoSecurityFeature is the per-feature status envelope inside
// security_and_analysis. gh emits `{status: "enabled"|"disabled"}` per
// feature; we stub everything to disabled for now.
type repoSecurityFeature struct {
	Status string `json:"status"`
}

// repoResponse mirrors GitHub's repo shape. The S62 audit (B14)
// added the nested owner envelope + html_url + topics + license +
// language + size + pushed_at. Legacy flat fields stay during the
// transition so existing clients keep parsing; new clients should
// consume the envelopes / gh-canonical fields.
type repoResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	// I7c (audit-I18): the legacy `owner_login` and `owner_type` flat
	// fields were dropped — read owner.login + owner.type instead. The
	// nested envelope below is the single source of truth.
	Owner         *repoOwnerEnvelope `json:"owner,omitempty"`
	Description   string             `json:"description"`
	Homepage      string             `json:"homepage"`
	Visibility    string             `json:"visibility"`
	Private       bool               `json:"private"`
	HTMLURL       string             `json:"html_url,omitempty"`
	DefaultBranch string             `json:"default_branch"`
	Fork          bool               `json:"fork"`
	Archived      bool               `json:"archived"`
	IsTemplate    bool               `json:"is_template"`
	HasIssues     bool               `json:"has_issues"`
	HasPulls      bool               `json:"has_pulls"`
	StarCount     int64              `json:"star_count"`
	WatcherCount  int64              `json:"watcher_count"`
	ForkCount     int64              `json:"fork_count"`
	// I7c (audit-I16): topics is always emitted as `[]` even when no
	// rows exist (no omitempty). Pre-fix scripts had to handle both
	// `null` and `[]` for the same conceptual empty list.
	Topics   []string             `json:"topics"`
	License  *repoLicenseEnvelope `json:"license,omitempty"`
	Language string               `json:"language,omitempty"`
	// Size is reported in KB to match gh-compat; the DB stores bytes.
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// PushedAt is best-effort: we don't track a separate push time
	// column, so emit updated_at. Active repos see them converge in
	// gh-compat anyway.
	PushedAt string `json:"pushed_at,omitempty"`

	// ─── I7a (audit-I11): gh-compat field expansion ──────────────────
	//
	// NodeID is the opaque base64 of `gid://shithub/Repository/{id}`.
	// gh-compat clients use it as the cache key for the repo node.
	NodeID string `json:"node_id,omitempty"`
	// Parent is set on fork responses: the full repo envelope of the
	// upstream the fork was created from. Nil for non-fork repos.
	Parent *repoResponse `json:"parent,omitempty"`
	// Permissions is populated for single-repo GETs against an
	// authenticated request. List endpoints omit it (gh-compat:
	// /users/{u}/repos doesn't carry permissions per row).
	Permissions *RepoPermissionsForResponse `json:"permissions,omitempty"`
	// SubscribersCount is people watching for notifications. On
	// shithub this is what the row's WatcherCount column tracks. gh's
	// WatcherCount field is a legacy alias for stars, which is why
	// the JSON tag below pins gh-canonical naming.
	SubscribersCount int64 `json:"subscribers_count"`
	// NetworkCount is the total descendants of this repo's fork graph.
	// For now stub to ForkCount (direct children only); a recursive
	// CTE would land in a follow-up perf review.
	NetworkCount int64 `json:"network_count"`

	// gh-compat merge-strategy toggles. Three of these are real (squash
	// / rebase / merge-commit toggles already on the row); the rest are
	// gh-compat defaults emitted as constants since shithub doesn't
	// gate behavior on them yet.
	AllowSquashMerge          bool `json:"allow_squash_merge"`
	AllowRebaseMerge          bool `json:"allow_rebase_merge"`
	AllowMergeCommit          bool `json:"allow_merge_commit"`
	AllowAutoMerge            bool `json:"allow_auto_merge"`
	AllowUpdateBranch         bool `json:"allow_update_branch"`
	DeleteBranchOnMerge       bool `json:"delete_branch_on_merge"`
	UseSquashPRTitleAsDefault bool `json:"use_squash_pr_title_as_default"`
	WebCommitSignoffRequired  bool `json:"web_commit_signoff_required"`

	// Merge-commit format selectors. gh-compat string enums that
	// describe how merge commit titles/messages are templated. Stubbed
	// to gh's documented defaults since shithub doesn't honor them
	// (single fixed template at merge time).
	SquashMergeCommitTitle   string `json:"squash_merge_commit_title"`
	SquashMergeCommitMessage string `json:"squash_merge_commit_message"`
	MergeCommitTitle         string `json:"merge_commit_title"`
	MergeCommitMessage       string `json:"merge_commit_message"`

	// MirrorURL is non-null when the repo is a remote mirror. shithub
	// doesn't support mirrored repos; emit `null` explicitly so the
	// field is present (gh-compat clients null-check).
	MirrorURL *string `json:"mirror_url"`
	// TemplateRepository is non-null when the repo was created from a
	// template repo. shithub's `repo create --template` flow records
	// the source but the envelope is deferred until I10's template
	// surface lands; emit `null` for now.
	TemplateRepository *repoResponse `json:"template_repository"`

	// SecurityAndAnalysis is a stub envelope (all features disabled).
	// See repoSecurityAndAnalysis.
	SecurityAndAnalysis *repoSecurityAndAnalysis `json:"security_and_analysis,omitempty"`
}

// RepoPermissionsForResponse is the JSON projection of the gh-compat
// permissions bundle. Mirrors policy.RepoPermissions field-for-field;
// the indirection keeps the API package from importing the policy
// struct's tags transitively.
type RepoPermissionsForResponse struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// gh-compat default constants for the merge-commit format selectors.
// Mirror what gh's API documents for new-repo defaults — clients that
// feature-detect on these strings see the gh shape.
const (
	ghCompatSquashMergeCommitTitle   = "COMMIT_OR_PR_TITLE"
	ghCompatSquashMergeCommitMessage = "COMMIT_MESSAGES"
	ghCompatMergeCommitTitle         = "MERGE_MESSAGE"
	ghCompatMergeCommitMessage       = "PR_TITLE"
)

// presentRepo builds the gh-compat response for one repo. Topics +
// baseURL are passed in so the function stays pure; callers that
// don't have them (e.g. legacy code paths) can supply nil + "" to get
// a response without those fields populated.
//
// For list endpoints, prefer this directly — permissions and parent
// stay nil (gh-compat: list pages omit them). For single-repo GETs
// against an authenticated actor, prefer presentRepoFull, which adds
// the per-actor permissions bundle and resolves the fork parent.
func presentRepo(r reposdb.Repo, ownerLogin string, topics []string, baseURL string) repoResponse {
	return presentRepoFull(r, ownerLogin, topics, nil, nil, baseURL)
}

// presentRepoFull is presentRepo + the per-actor permissions bundle
// + the fork parent envelope (nil for non-forks). Single-repo GETs
// call through here so the response carries the full gh-compat
// surface; list endpoints call presentRepo so they stay light.
func presentRepoFull(
	r reposdb.Repo,
	ownerLogin string,
	topics []string,
	parent *repoResponse,
	perms *policy.RepoPermissions,
	baseURL string,
) repoResponse {
	ownerType := "user"
	if r.OwnerOrgID.Valid {
		ownerType = "org"
	}
	repoRef := policy.NewRepoRefFromRepo(r)
	resp := repoResponse{
		ID:       r.ID,
		Name:     r.Name,
		FullName: ownerLogin + "/" + r.Name,
		Owner: &repoOwnerEnvelope{
			Login: ownerLogin,
			Type:  capitalizeFirst(ownerType),
		},
		Description:   r.Description,
		Homepage:      r.Homepage,
		Visibility:    string(r.Visibility),
		Private:       repoRef.IsPrivate(),
		DefaultBranch: r.DefaultBranch,
		Fork:          r.ForkOfRepoID.Valid,
		Archived:      r.IsArchived,
		IsTemplate:    r.IsTemplate,
		HasIssues:     r.HasIssues,
		HasPulls:      r.HasPulls,
		StarCount:     r.StarCount,
		WatcherCount:  r.WatcherCount,
		ForkCount:     r.ForkCount,
		Size:          r.DiskUsedBytes / 1024,
		CreatedAt:     r.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:     r.UpdatedAt.Time.UTC().Format(time.RFC3339),
		PushedAt:      r.UpdatedAt.Time.UTC().Format(time.RFC3339),

		// I7a — gh-compat expansion.
		NodeID:           repoNodeID(r.ID),
		Parent:           parent,
		SubscribersCount: r.WatcherCount, // shithub's watcher_count column == gh's subscribers_count
		NetworkCount:     r.ForkCount,    // direct-child count; recursive CTE deferred to follow-up perf review

		AllowSquashMerge:          r.AllowSquashMerge,
		AllowRebaseMerge:          r.AllowRebaseMerge,
		AllowMergeCommit:          r.AllowMergeCommit,
		AllowAutoMerge:            false, // deferred — no behavior yet
		AllowUpdateBranch:         false, // deferred — no behavior yet
		DeleteBranchOnMerge:       r.DeleteBranchOnMerge,
		UseSquashPRTitleAsDefault: false, // deferred — no behavior yet
		WebCommitSignoffRequired:  false, // deferred — no behavior yet

		SquashMergeCommitTitle:   ghCompatSquashMergeCommitTitle,
		SquashMergeCommitMessage: ghCompatSquashMergeCommitMessage,
		MergeCommitTitle:         ghCompatMergeCommitTitle,
		MergeCommitMessage:       ghCompatMergeCommitMessage,

		MirrorURL:          nil, // shithub doesn't mirror; null explicit
		TemplateRepository: nil, // deferred until I10
		SecurityAndAnalysis: &repoSecurityAndAnalysis{
			SecretScanning:               repoSecurityFeature{Status: "disabled"},
			SecretScanningPushProtection: repoSecurityFeature{Status: "disabled"},
			DependabotSecurityUpdates:    repoSecurityFeature{Status: "disabled"},
		},
	}
	if r.OwnerUserID.Valid {
		resp.Owner.ID = r.OwnerUserID.Int64
	} else if r.OwnerOrgID.Valid {
		resp.Owner.ID = r.OwnerOrgID.Int64
	}
	if r.LicenseKey.Valid && r.LicenseKey.String != "" {
		key := r.LicenseKey.String
		resp.License = &repoLicenseEnvelope{
			Key:    strings.ToLower(key),
			SPDXID: key,
			Name:   templates.LicenseName(key),
			URL:    licenseURL(baseURL, key),
			NodeID: licenseNodeID(key),
		}
	}
	if r.PrimaryLanguage.Valid {
		resp.Language = r.PrimaryLanguage.String
	}
	// I7c (audit-I16): empty topic list still emits `[]` not null.
	if topics == nil {
		topics = []string{}
	}
	resp.Topics = topics
	if baseURL != "" {
		resp.HTMLURL = strings.TrimRight(baseURL, "/") + "/" + ownerLogin + "/" + r.Name
	}
	if perms != nil {
		resp.Permissions = &RepoPermissionsForResponse{
			Admin:    perms.Admin,
			Maintain: perms.Maintain,
			Push:     perms.Push,
			Triage:   perms.Triage,
			Pull:     perms.Pull,
		}
	}
	return resp
}

// repoNodeID returns the opaque base64 ID for a repository node.
// Format mirrors gh's GraphQL node id shape: base64(`gid://shithub/
// Repository/{numeric-id}`). Clients should treat this as opaque.
func repoNodeID(id int64) string {
	raw := "gid://shithub/Repository/" + strconv.FormatInt(id, 10)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// licenseURL builds the discovery URL for a license key. Empty baseURL
// returns "" so list responses (which don't carry baseURL) skip the
// field via omitempty.
func licenseURL(baseURL, spdxID string) string {
	if baseURL == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/api/v1/licenses/" + strings.ToLower(spdxID)
}

// licenseNodeID is the opaque base64 of `gid://shithub/License/{key}`.
func licenseNodeID(spdxID string) string {
	raw := "gid://shithub/License/" + spdxID
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// topicsFor fetches the topic set for a repo. Returns nil on lookup
// failure (best-effort: a topic lookup error must not break the
// repo response).
func (h *Handlers) topicsFor(ctx context.Context, repoID int64) []string {
	rows, err := reposdb.New().ListRepoTopics(ctx, h.d.Pool, repoID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return rows
}

// capitalizeFirst returns s with its first rune upper-cased. Used to
// project the internal lowercase owner_type ("user"|"org") onto the
// GitHub-compat title-case form ("User"|"Organization"-ish — we use
// "User" / "Organization" exactly).
func capitalizeFirst(s string) string {
	switch s {
	case "user":
		return "User"
	case "org":
		return "Organization"
	}
	return s
}

// ─── list endpoints ─────────────────────────────────────────────────

func (h *Handlers) userReposList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := reposdb.New()
	total, err := q.CountReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: auth.UserID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count user repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListReposForOwnerUserPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerUserPagedParams{
		OwnerUserID: pgtype.Int8{Int64: auth.UserID, Valid: true},
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows = filterReposByVisibility(rows, visibility)
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, auth.Username)
}

// strictVisibility validates the `visibility` query parameter. Empty
// returns ("", nil) — no filter. Anything outside
// {public, private, internal} is 422 (E15: pre-fix would silently
// return all repos for unknown values).
func strictVisibility(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "public", "private", "internal":
		return strings.ToLower(strings.TrimSpace(s)), nil
	default:
		return "", fmt.Errorf("visibility: must be public, private, or internal (got %q)", s)
	}
}

// filterReposByVisibility narrows the row set when a visibility filter
// is present. Empty filter returns rows unchanged.
func filterReposByVisibility(rows []reposdb.Repo, visibility string) []reposdb.Repo {
	if visibility == "" {
		return rows
	}
	filtered := rows[:0]
	for _, row := range rows {
		if strings.EqualFold(string(row.Visibility), visibility) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (h *Handlers) userPublicReposList(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.resolveAPIUserOwner(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	q := reposdb.New()
	// Self-view of /users/{me}/repos shows everything (private included),
	// matching GitHub's behavior.
	if auth.UserID == owner.ID {
		page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
		total, err := q.CountReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: owner.ID, Valid: true})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: count user repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows, err := q.ListReposForOwnerUserPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerUserPagedParams{
			OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
			Limit:       int32(perPage),
			Offset:      int32((page - 1) * perPage),
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: list user repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows = filterReposByVisibility(rows, visibility)
		h.writeRepoListPage(w, r, page, perPage, int(total), rows, owner.Username)
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total, err := q.CountPublicReposForOwnerUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: owner.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count public repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListPublicReposForOwnerUser(r.Context(), h.d.Pool, reposdb.ListPublicReposForOwnerUserParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list public repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows = filterReposByVisibility(rows, visibility)
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, owner.Username)
}

func (h *Handlers) orgReposList(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrgOwner(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	visibility, verr := strictVisibility(r.URL.Query().Get("visibility"))
	if verr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, verr.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	q := reposdb.New()

	memberView := false
	if auth.UserID != 0 {
		isMem, err := orgs.IsMember(r.Context(), orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, org.ID, auth.UserID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: org member check", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		memberView = isMem || auth.IsSiteAdmin
	}

	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	if memberView {
		total, err := q.CountReposForOwnerOrg(r.Context(), h.d.Pool, pgtype.Int8{Int64: org.ID, Valid: true})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: count org repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows, err := q.ListReposForOwnerOrgPaged(r.Context(), h.d.Pool, reposdb.ListReposForOwnerOrgPagedParams{
			OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
			Limit:      int32(perPage),
			Offset:     int32((page - 1) * perPage),
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: list org repos", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		rows = filterReposByVisibility(rows, visibility)
		h.writeRepoListPage(w, r, page, perPage, int(total), rows, string(org.Slug))
		return
	}
	total, err := q.CountPublicReposForOwnerOrg(r.Context(), h.d.Pool, pgtype.Int8{Int64: org.ID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count public org repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListPublicReposForOwnerOrg(r.Context(), h.d.Pool, reposdb.ListPublicReposForOwnerOrgParams{
		OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
		Limit:      int32(perPage),
		Offset:     int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list public org repos", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows = filterReposByVisibility(rows, visibility)
	h.writeRepoListPage(w, r, page, perPage, int(total), rows, string(org.Slug))
}

func (h *Handlers) writeRepoListPage(w http.ResponseWriter, r *http.Request, page, perPage, total int, rows []reposdb.Repo, ownerLogin string) {
	link := apipage.Page{
		Current: page, PerPage: perPage, Total: total,
	}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]repoResponse, 0, len(rows))
	for _, row := range rows {
		// List endpoint: skip topics lookup to avoid N+1. The single
		// GET path populates them; CLI list views don't render them.
		out = append(out, presentRepo(row, ownerLogin, nil, h.d.BaseURL))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── single-repo GET ────────────────────────────────────────────────

func (h *Handlers) repoGet(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	// I7a: single-repo GET carries the full gh-compat surface — fork
	// parent (when the repo is itself a fork) plus the actor's
	// permission bundle. Both lookups are best-effort: a parent miss
	// (parent repo deleted, transient pool error) emits the response
	// without parent rather than failing the GET.
	parent := h.resolveForkParent(r.Context(), repo)
	auth := middleware.PATAuthFromContext(r.Context())
	perms := policy.PermissionsFor(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.NewRepoRefFromRepo(repo))
	writeJSON(w, http.StatusOK, presentRepoFull(repo, ownerLogin, h.topicsFor(r.Context(), repo.ID), parent, &perms, h.d.BaseURL))
}

// resolveForkParent returns the gh-compat parent envelope for a fork,
// or nil when the repo isn't a fork or the parent lookup fails. Lookup
// failure is silent by design — the parent column is best-effort and
// must not break the GET.
func (h *Handlers) resolveForkParent(ctx context.Context, child reposdb.Repo) *repoResponse {
	if !child.ForkOfRepoID.Valid {
		return nil
	}
	q := reposdb.New()
	parent, err := q.GetRepoByID(ctx, h.d.Pool, child.ForkOfRepoID.Int64)
	if err != nil || parent.DeletedAt.Valid {
		return nil
	}
	parentLogin, err := h.resolveOwnerLogin(ctx, parent)
	if err != nil {
		return nil
	}
	envelope := presentRepo(parent, parentLogin, nil, h.d.BaseURL)
	return &envelope
}

// resolveOwnerLogin returns the owner slug for a repo by joining
// against users or orgs. Mirrors what lookupRepoByLogin returns at
// the top of a request, but works from a Repo row already in hand.
func (h *Handlers) resolveOwnerLogin(ctx context.Context, r reposdb.Repo) (string, error) {
	row, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, h.d.Pool, r.ID)
	if err != nil {
		return "", err
	}
	// sqlc projects COALESCE(varchar, varchar) as interface{}; the row
	// always carries a string at runtime since the LEFT JOINs guarantee
	// exactly one of users.username or orgs.slug is non-NULL.
	login, _ := row.OwnerUsername.(string)
	return login, nil
}

// ─── create endpoints ───────────────────────────────────────────────

type repoCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	Private     *bool  `json:"private,omitempty"`
	AutoInit    bool   `json:"auto_init"`
	License     string `json:"license_template"`
	Gitignore   string `json:"gitignore_template"`
}

// resolvedVisibility picks "public" or "private" from a request, honoring
// either `visibility` (preferred, matches our internal vocab) or the
// gh-compatible `private` boolean. Defaults to "private" — safer than
// public.
func (req repoCreateRequest) resolvedVisibility() (string, error) {
	if req.Visibility != "" {
		switch strings.ToLower(req.Visibility) {
		case "public", "private":
			return strings.ToLower(req.Visibility), nil
		default:
			return "", errors.New("visibility must be public or private")
		}
	}
	if req.Private != nil {
		if *req.Private {
			return "private", nil
		}
		return "public", nil
	}
	return "private", nil
}

func (h *Handlers) userRepoCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body repoCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	visibility, err := body.resolvedVisibility()
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	params := repos.Params{
		ActorUserID:      auth.UserID,
		ActorIsSiteAdmin: auth.IsSiteAdmin,
		OwnerUserID:      auth.UserID,
		OwnerUsername:    auth.Username,
		// H3 (H12): byte-exact. Pre-fix `repos.NormalizeName` trimmed
		// whitespace and lowercased — `repo create "  Demo  "` was
		// silently saved as "demo". ValidateName (called inside
		// repos.Create) already rejects uppercase + whitespace via the
		// `[a-z0-9]`-edged regex, so pass the verbatim user input and
		// let it 422 rather than masking typos.
		Name:         body.Name,
		Description:  body.Description,
		Visibility:   visibility,
		InitReadme:   body.AutoInit,
		LicenseKey:   body.License,
		GitignoreKey: body.Gitignore,
	}
	h.runRepoCreate(w, r, params, auth.Username)
}

func (h *Handlers) orgRepoCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	org, ok := h.resolveAPIOrgOwner(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	odeps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
	isMember, err := orgs.IsMember(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org member check", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if !isMember && !auth.IsSiteAdmin {
		// Existence-leak parity with the rest of the surface.
		writeAPIError(w, http.StatusNotFound, "org not found")
		return
	}
	isOwner, err := orgs.IsOwner(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org owner check", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if !isOwner && !org.AllowMemberRepoCreate && !auth.IsSiteAdmin {
		writeAPIError(w, http.StatusForbidden, "organization restricts repo creation to owners")
		return
	}
	var body repoCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	visibility, err := body.resolvedVisibility()
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	params := repos.Params{
		ActorUserID:      auth.UserID,
		ActorIsSiteAdmin: auth.IsSiteAdmin,
		OwnerOrgID:       org.ID,
		OwnerSlug:        string(org.Slug),
		// H3 (H12): byte-exact — see user-repo path above.
		Name:         body.Name,
		Description:  body.Description,
		Visibility:   visibility,
		InitReadme:   body.AutoInit,
		LicenseKey:   body.License,
		GitignoreKey: body.Gitignore,
	}
	h.runRepoCreate(w, r, params, string(org.Slug))
}

func (h *Handlers) runRepoCreate(w http.ResponseWriter, r *http.Request, params repos.Params, ownerLogin string) {
	if h.d.Audit == nil || h.d.Throttle == nil || h.d.RepoFS == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "repo create is not configured")
		return
	}
	res, err := repos.Create(r.Context(), repos.Deps{
		Pool:         h.d.Pool,
		RepoFS:       h.d.RepoFS,
		Audit:        h.d.Audit,
		Limiter:      h.d.Throttle,
		Logger:       h.d.Logger,
		ShithubdPath: h.d.ShithubdPath,
	}, params)
	if err != nil {
		writeRepoCreateError(w, err)
		return
	}
	// Brand-new repo — no topics yet, skip the lookup.
	writeJSON(w, http.StatusCreated, presentRepo(res.Repo, ownerLogin, nil, h.d.BaseURL))
}

func writeRepoCreateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repos.ErrInvalidName),
		errors.Is(err, repos.ErrReservedName),
		errors.Is(err, repos.ErrDescriptionTooLong),
		errors.Is(err, repos.ErrUnknownLicense),
		errors.Is(err, repos.ErrUnknownGitignore):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, repos.ErrTaken):
		writeAPIError(w, http.StatusConflict, "name taken for owner")
	case errors.Is(err, repos.ErrNoVerifiedEmail):
		writeAPIError(w, http.StatusUnprocessableEntity, "actor has no verified primary email")
	case errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded):
		writeAPIError(w, http.StatusPaymentRequired, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "create failed")
	}
}

// ─── update / delete ────────────────────────────────────────────────

type repoPatchRequest struct {
	Description *string `json:"description,omitempty"`
	// Homepage persists the repo's homepage URL (E7). Pre-fix the
	// field was silently dropped because the column didn't exist;
	// migration 0116 adds the column, and PATCH now round-trips it
	// through the same general-settings UPDATE.
	Homepage   *string `json:"homepage,omitempty"`
	HasIssues  *bool   `json:"has_issues,omitempty"`
	HasPulls   *bool   `json:"has_pulls,omitempty"`
	Archived   *bool   `json:"archived,omitempty"`
	Visibility *string `json:"visibility,omitempty"`
	// Name dispatches into lifecycle.Rename when non-nil. Closes
	// audit finding C7: the previous behavior silently dropped the
	// field, letting `shithub repo rename` print "Renamed to <old
	// name>" against a no-op response.
	Name *string `json:"name,omitempty"`
	// DefaultBranch swaps the repo's default branch. The named branch
	// must exist (validated via repogit.ListRefs); unknown branches
	// return 422 rather than silently no-op'ing. E28.
	DefaultBranch *string `json:"default_branch,omitempty"`
}

func (h *Handlers) repoPatch(w http.ResponseWriter, r *http.Request) {
	// E9: decode body BEFORE running the archive-aware policy gate so
	// we can pick the right action. The gate normally uses
	// ActionRepoSettingsGeneral, which is blocked on archived repos —
	// that locked unarchive out and turned archive into a one-way
	// trap. When the body's only change is `archived: false`, route
	// the gate through ActionRepoArchive (which is exempt from the
	// archive-write block, see policy.go § 8).
	var body repoPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	gateAction := policy.ActionRepoSettingsGeneral
	if body.Archived != nil {
		gateAction = policy.ActionRepoArchive
	}
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, gateAction)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	// C7: dispatch rename through the existing lifecycle.Rename
	// pipeline (validate → tx insert-redirect + UPDATE → FS move →
	// audit). Rename requires repo admin (matches the HTML form
	// gate); the outer ActionRepoSettingsGeneral check already passed
	// for the broader PATCH, so we add the stricter check only when
	// the name field is present.
	if body.Name != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(),
			policy.ActionRepoAdmin, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "rename requires repo admin")
			return
		}
		ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
		err := lifecycle.Rename(r.Context(), ldeps, lifecycle.RenameParams{
			ActorUserID: auth.UserID,
			RepoID:      repo.ID,
			OwnerUserID: repo.OwnerUserID.Int64,
			OwnerName:   ownerLogin,
			OldName:     repo.Name,
			NewName:     *body.Name,
		})
		if err != nil {
			h.writeRenameError(w, r, err)
			return
		}
		// Reload so the returned repo reflects the new name, and so any
		// sibling fields the caller patched in the same request observe
		// the rename'd row state. Repo struct is captured by-value above
		// (used by the visibility/archived branches); refresh the local
		// `repo` and let the rest of the handler keep working.
		fresh, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, repo.ID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: refetch after rename", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "reload failed")
			return
		}
		repo = fresh
		policy.InvalidateRepo(r.Context(), repo.ID)
	}
	// General settings (description, has_issues, has_pulls, homepage)
	// go through the single UpdateRepoGeneralSettings query so the
	// form-driven HTML surface and this REST path observe the same row
	// updates. Each pointer field defaults to the current value so a
	// PATCH carrying only one knob doesn't blank the others.
	if body.Description != nil || body.HasIssues != nil || body.HasPulls != nil || body.Homepage != nil {
		desc := repo.Description
		if body.Description != nil {
			if err := repos.ValidateDescription(*body.Description); err != nil {
				writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			desc = *body.Description
		}
		hasIssues := repo.HasIssues
		if body.HasIssues != nil {
			hasIssues = *body.HasIssues
		}
		hasPulls := repo.HasPulls
		if body.HasPulls != nil {
			hasPulls = *body.HasPulls
		}
		homepage := repo.Homepage
		if body.Homepage != nil {
			h := strings.TrimSpace(*body.Homepage)
			if len(h) > 255 {
				writeAPIError(w, http.StatusUnprocessableEntity, "homepage too long (max 255)")
				return
			}
			homepage = h
		}
		if err := reposdb.New().UpdateRepoGeneralSettings(r.Context(), h.d.Pool, reposdb.UpdateRepoGeneralSettingsParams{
			ID:          repo.ID,
			Description: desc,
			HasIssues:   hasIssues,
			HasPulls:    hasPulls,
			Homepage:    homepage,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: repo patch general", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	if body.Archived != nil {
		ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
		wantArchived := *body.Archived
		currentlyArchived := repo.IsArchived
		switch {
		case wantArchived && !currentlyArchived:
			if err := lifecycle.Archive(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: archive", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "archive failed")
				return
			}
		case !wantArchived && currentlyArchived:
			if err := lifecycle.Unarchive(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: unarchive", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "unarchive failed")
				return
			}
		}
	}
	if body.Visibility != nil {
		newVis := strings.ToLower(*body.Visibility)
		if newVis != "public" && newVis != "private" {
			writeAPIError(w, http.StatusUnprocessableEntity, "visibility must be public or private")
			return
		}
		if newVis != string(repo.Visibility) {
			ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
			if err := lifecycle.SetVisibility(r.Context(), ldeps, auth.UserID, repo.ID, newVis); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: set visibility", "error", err)
				if errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
					writeAPIError(w, http.StatusPaymentRequired, err.Error())
					return
				}
				writeAPIError(w, http.StatusInternalServerError, "visibility update failed")
				return
			}
		}
	}
	if body.DefaultBranch != nil {
		newDefault := strings.TrimSpace(*body.DefaultBranch)
		if newDefault == "" {
			writeAPIError(w, http.StatusUnprocessableEntity, "default_branch must not be empty")
			return
		}
		if newDefault != repo.DefaultBranch {
			gitDir, gerr := h.d.RepoFS.RepoPath(ownerLogin, repo.Name)
			if gerr != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: default-branch repo path", "error", gerr)
				writeAPIError(w, http.StatusInternalServerError, "default_branch update failed")
				return
			}
			refs, rerr := repogit.ListRefs(r.Context(), gitDir)
			if rerr != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: default-branch ref list", "error", rerr)
				writeAPIError(w, http.StatusInternalServerError, "default_branch update failed")
				return
			}
			found := false
			for _, b := range refs.Branches {
				if b.Name == newDefault {
					found = true
					break
				}
			}
			if !found {
				writeAPIError(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("branch %q not found", newDefault))
				return
			}
			if err := reposdb.New().UpdateRepoDefaultBranch(r.Context(), h.d.Pool, reposdb.UpdateRepoDefaultBranchParams{
				ID: repo.ID, DefaultBranch: newDefault,
			}); err != nil {
				h.d.Logger.ErrorContext(r.Context(), "api: update default branch", "error", err)
				writeAPIError(w, http.StatusInternalServerError, "default_branch update failed")
				return
			}
			// On-disk HEAD update mirrors the HTML settings handler — DB
			// is the source of truth; if the symbolic-ref step fails we
			// log and keep going so the user's UI reflects the change.
			if err := repogit.SetSymbolicRef(r.Context(), gitDir, "HEAD", "refs/heads/"+newDefault); err != nil {
				h.d.Logger.WarnContext(r.Context(), "api: default-branch symbolic-ref", "error", err)
			}
		}
	}
	// Re-load the freshest copy so the response reflects all four
	// possible updates in a single payload.
	fresh, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: refetch after patch", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, presentRepo(fresh, ownerLogin, h.topicsFor(r.Context(), fresh.ID), h.d.BaseURL))
}

func (h *Handlers) repoDelete(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionRepoDelete)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	ldeps := lifecycle.Deps{Pool: h.d.Pool, RepoFS: h.d.RepoFS, Audit: h.d.Audit, Logger: h.d.Logger}
	if err := lifecycle.SoftDelete(r.Context(), ldeps, auth.UserID, repo.ID); err != nil {
		if errors.Is(err, lifecycle.ErrAlreadyDeleted) {
			writeAPIError(w, http.StatusNotFound, "repo not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: soft delete", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRenameError maps lifecycle.Rename errors onto REST status
// codes. Distinct from the HTML lifecycleError mapper because the
// REST surface returns JSON-envelope errors via writeAPIError and
// uses 422 (validation) / 409 (conflict) shapes gh-compat clients
// recognize, where the HTML form uses 400 with plain text.
func (h *Handlers) writeRenameError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrSameName):
		writeAPIError(w, http.StatusUnprocessableEntity, "new name equals current name")
	case errors.Is(err, lifecycle.ErrInvalidName):
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid repository name")
	case errors.Is(err, lifecycle.ErrReservedName):
		writeAPIError(w, http.StatusUnprocessableEntity, "reserved repository name")
	case errors.Is(err, lifecycle.ErrNameTaken):
		writeAPIError(w, http.StatusConflict, "name already taken on this owner")
	case errors.Is(err, lifecycle.ErrRenameRateLimited):
		writeAPIError(w, http.StatusTooManyRequests, "rename rate limit (5 per 30 days) exceeded")
	default:
		h.d.Logger.ErrorContext(r.Context(), "api: rename", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "rename failed")
	}
}

// ─── resolvers ──────────────────────────────────────────────────────

func (h *Handlers) resolveAPIUserOwner(w http.ResponseWriter, r *http.Request, username string) (usersdb.User, bool) {
	user, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "user not found")
			return usersdb.User{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup user", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return usersdb.User{}, false
	}
	return user, true
}

func (h *Handlers) resolveAPIOrgOwner(w http.ResponseWriter, r *http.Request, slug string) (orgsdb.Org, bool) {
	org, err := orgsdb.New().GetOrgBySlug(r.Context(), h.d.Pool, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "org not found")
			return orgsdb.Org{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup org", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return orgsdb.Org{}, false
	}
	if org.DeletedAt.Valid {
		writeAPIError(w, http.StatusNotFound, "org not found")
		return orgsdb.Org{}, false
	}
	return org, true
}

// resolveAPIRepoWithLogin loads {owner}/{repo}, runs the policy gate
// (404-on-deny), and additionally returns the owner's login string for
// rendering `full_name`. The login lookup is one extra DB round-trip per
// request — fine for a non-hot path. We compose on top of the existing
// resolveAPIRepo so the existence-leak treatment stays identical.
func (h *Handlers) resolveAPIRepoWithLogin(w http.ResponseWriter, r *http.Request, action policy.Action) (reposdb.Repo, string, bool) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 && actionRequiresAuth(action) {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return reposdb.Repo{}, "", false
	}
	ownerLogin := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	repo, login, err := lookupRepoByLogin(r, h.d.Pool, ownerLogin, repoName)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, "", false
	}
	decision := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), action, policy.NewRepoRefFromRepo(repo))
	if !decision.Allow {
		writeAPIDenial(w, decision)
		return reposdb.Repo{}, "", false
	}
	return repo, login, true
}

// writeAPIDenial maps a policy.Decision deny code onto an HTTP status.
// E18: prior to this, every deny rendered as 404 "repo not found",
// making archived/paused/org-suspended states indistinguishable from
// deleted. The 404 path stays for visibility denies (private repos —
// existence leak guard), but DenyArchived/Paused/OrgSuspended return
// 403 with a specific message so clients can do something about it.
func writeAPIDenial(w http.ResponseWriter, d policy.Decision) {
	switch d.Code {
	case policy.DenyArchived:
		writeAPIError(w, http.StatusForbidden, "repository is archived")
	case policy.DenyPaused:
		writeAPIError(w, http.StatusPaymentRequired, "repository is paused")
	case policy.DenyOrgSuspended:
		writeAPIError(w, http.StatusForbidden, "owning organization is suspended")
	default:
		// DenyVisibility, DenyAnonymous, DenyRoleTooLow, etc. all fall
		// through to 404 to avoid leaking that the repo exists when
		// the caller can't see it.
		writeAPIError(w, http.StatusNotFound, "repo not found")
	}
}

// actionRequiresAuth returns true for actions that always require a
// logged-in caller. Read-shaped actions pass through anonymously so
// the visibility gate inside policy.Can does the talking.
func actionRequiresAuth(a policy.Action) bool {
	switch a {
	case policy.ActionRepoRead, policy.ActionIssueRead, policy.ActionPullRead:
		return false
	default:
		return true
	}
}

// lookupRepoByLogin tries the user-owner path first, then the org-owner
// path. The login string returned is whichever resolved successfully so
// the caller can plug it into the full_name field.
//
// PRO-EXT01-11b: enforces PAT repo binding. If the request is
// authenticated via a token bound to a different repo, the resolution
// returns pgx.ErrNoRows so handlers naturally 404 — preserving the
// "this repo doesn't exist from your perspective" semantic without
// leaking that the binding was the actual reason.
func lookupRepoByLogin(r *http.Request, pool reposdbPool, ownerLogin, repoName string) (reposdb.Repo, string, error) {
	rq := reposdb.New()
	if user, err := usersdb.New().GetUserByUsername(r.Context(), pool, ownerLogin); err == nil {
		repo, err := rq.GetRepoByOwnerUserAndName(r.Context(), pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: user.ID, Valid: true},
			Name:        repoName,
		})
		if err == nil {
			if !patBindingAllowsRepo(r, repo.ID) {
				return reposdb.Repo{}, "", pgx.ErrNoRows
			}
			return repo, user.Username, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return reposdb.Repo{}, "", err
		}
	}
	if org, err := orgsdb.New().GetOrgBySlug(r.Context(), pool, ownerLogin); err == nil {
		repo, err := rq.GetRepoByOwnerOrgAndName(r.Context(), pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
			Name:       repoName,
		})
		if err == nil {
			if !patBindingAllowsRepo(r, repo.ID) {
				return reposdb.Repo{}, "", pgx.ErrNoRows
			}
			return repo, string(org.Slug), nil
		}
	}
	return reposdb.Repo{}, "", pgx.ErrNoRows
}

// patBindingAllowsRepo reports whether the request's PAT auth (if any)
// permits acting on the given repo. Pure-session requests (no PAT auth)
// always allow.
func patBindingAllowsRepo(r *http.Request, repoID int64) bool {
	auth := middleware.PATAuthFromContext(r.Context())
	return pat.RepoBindingAllows(auth.RepoBinding, repoID)
}

// reposdbPool aliases the pgx DBTX interface that all sqlc-generated
// methods accept; declaring it here keeps this file from importing
// pgxpool directly for what is effectively a typed parameter.
type reposdbPool = reposdb.DBTX
