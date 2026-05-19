// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/fork"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountReposFollowups registers the S50 §2 follow-up endpoints that
// round out the repos REST surface: README fetch, topics replace +
// clear, and fork-sync.
//
//	GET    /api/v1/repos/{o}/{r}/readme[?ref=]
//	PUT    /api/v1/repos/{o}/{r}/topics
//	DELETE /api/v1/repos/{o}/{r}/topics
//	POST   /api/v1/repos/{o}/{r}/merge-upstream
//
// Scopes: `repo:read` on the README GET, `repo:write` on the
// mutations.
func (h *Handlers) mountReposFollowups(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/readme", h.repoReadmeGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/topics", h.repoTopicsReplace)
		r.Delete("/api/v1/repos/{owner}/{repo}/topics", h.repoTopicsClear)
		r.Post("/api/v1/repos/{owner}/{repo}/merge-upstream", h.repoMergeUpstream)
	})
}

// ─── README ─────────────────────────────────────────────────────────

type readmeResponse struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
}

const readmeMaxBytes = 1 << 20 // 1 MiB, matches the HTML render cap.

func (h *Handlers) repoReadmeGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: readme repo path", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}
	entries, err := git.LsTree(r.Context(), gitDir, ref, "")
	if err != nil {
		// E-audit E13: empty repos (no commits / no default-branch
		// pointer) used to surface as 500. The repo itself is fine —
		// the policy gate already confirmed it. Any ls-tree failure
		// here means "no tree to walk", which from the README
		// endpoint's perspective is equivalent to "no README". 404
		// matches gh + lets the CLI render an empty "no readme" panel.
		if errors.Is(err, git.ErrRefNotFound) || errors.Is(err, git.ErrPathNotFound) || errors.Is(err, git.ErrNotATree) {
			writeAPIError(w, http.StatusNotFound, "no README found")
			return
		}
		// Unknown errors: log and 404 too — the operator gets the
		// detail in logs; the client gets a clean shape rather than
		// a misleading 500. Real auth/path failures are caught above
		// by the policy gate, never here.
		h.d.Logger.InfoContext(r.Context(), "api: readme ls-tree empty/error → 404", "error", err)
		writeAPIError(w, http.StatusNotFound, "no README found")
		return
	}
	readmeName := pickREADME(entries)
	if readmeName == "" {
		writeAPIError(w, http.StatusNotFound, "no README found at ref")
		return
	}
	body, err := git.ReadBlobBytes(r.Context(), gitDir, ref, readmeName, readmeMaxBytes)
	if err != nil && !errors.Is(err, git.ErrBlobTooLarge) {
		h.d.Logger.ErrorContext(r.Context(), "api: readme read", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "read failed")
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	downloadURL := scheme + "://" + r.Host + "/" + chi.URLParam(r, "owner") + "/" + repo.Name + "/raw/" + ref + "/" + readmeName

	writeJSON(w, http.StatusOK, readmeResponse{
		Name:        readmeName,
		Path:        readmeName,
		Size:        int64(len(body)),
		Encoding:    "base64",
		Content:     base64.StdEncoding.EncodeToString(body),
		DownloadURL: downloadURL,
	})
}

// pickREADME returns the first entry whose name starts with "readme"
// (case-insensitive), preferring `.md` / `.markdown` over plain text
// to match the HTML code-view's choice when multiple README files
// exist at the same level (e.g. README.md + README.rst).
func pickREADME(entries []git.TreeEntry) string {
	var fallback string
	for _, e := range entries {
		if e.Kind != git.EntryBlob {
			continue
		}
		lower := strings.ToLower(e.Name)
		if !strings.HasPrefix(lower, "readme") {
			continue
		}
		if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
			return e.Name
		}
		if fallback == "" {
			fallback = e.Name
		}
	}
	return fallback
}

// ─── topics ─────────────────────────────────────────────────────────

type topicsRequest struct {
	Names []string `json:"names"`
}

type topicsResponse struct {
	Names []string `json:"names"`
}

func (h *Handlers) repoTopicsReplace(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	var body topicsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := repos.ReplaceTopics(r.Context(), h.reposDeps(), repo.ID, body.Names); err != nil {
		writeTopicsError(w, err)
		return
	}
	names, err := reposdb.New().ListRepoTopics(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, topicsResponse{Names: names})
}

func (h *Handlers) repoTopicsClear(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if err := repos.ReplaceTopics(r.Context(), h.reposDeps(), repo.ID, []string{}); err != nil {
		writeTopicsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeTopicsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repos.ErrTooManyTopics):
		writeAPIError(w, http.StatusUnprocessableEntity, "too many topics (max 20)")
	case errors.Is(err, repos.ErrInvalidTopic):
		writeAPIError(w, http.StatusUnprocessableEntity, "topic must be lowercase letters/digits/hyphens, 1-50 chars")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

// ─── merge-upstream (fork sync) ─────────────────────────────────────

type mergeUpstreamResponse struct {
	Merged     bool   `json:"merged"`
	OldOID     string `json:"old_oid,omitempty"`
	NewOID     string `json:"new_oid,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	Message    string `json:"message"`
}

func (h *Handlers) repoMergeUpstream(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if !repo.ForkOfRepoID.Valid {
		writeAPIError(w, http.StatusUnprocessableEntity, "not a fork")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	res, err := fork.Sync(r.Context(), h.forkDeps(), auth.UserID, repo.ID)
	if err != nil {
		writeForkSyncError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mergeUpstreamResponse{
		Merged:     res.OldOID != res.NewOID,
		OldOID:     res.OldOID,
		NewOID:     res.NewOID,
		BaseBranch: repo.DefaultBranch,
		Message:    "fast-forwarded to upstream",
	})
}

func writeForkSyncError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fork.ErrSyncUpToDate):
		// gh treats up-to-date as a 200 with merged=false; we surface
		// the same shape so client logic doesn't have to special-case.
		writeJSON(w, http.StatusOK, mergeUpstreamResponse{
			Merged:  false,
			Message: "already up to date",
		})
	case errors.Is(err, fork.ErrSyncDiverged):
		writeAPIError(w, http.StatusConflict, "fork has diverged from upstream; sync via your client")
	case errors.Is(err, fork.ErrSyncRefRaced):
		writeAPIError(w, http.StatusConflict, "ref changed concurrently; retry")
	case errors.Is(err, fork.ErrSyncDefaultsMissing):
		writeAPIError(w, http.StatusUnprocessableEntity, "source or fork default branch is empty")
	case errors.Is(err, fork.ErrForkNotInitialized):
		writeAPIError(w, http.StatusConflict, "fork is still being prepared; retry shortly")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

// reposDeps builds a repos.Deps from the handler's Deps. The topics
// orchestrator only needs Pool today; we pass the full set so future
// hooks (audit, throttle) plug in without re-touching call sites.
func (h *Handlers) reposDeps() repos.Deps {
	a := h.d.Audit
	if a == nil {
		a = audit.NewRecorder()
	}
	lim := h.d.Throttle
	if lim == nil {
		lim = throttle.NewLimiter()
	}
	return repos.Deps{
		Pool:    h.d.Pool,
		RepoFS:  h.d.RepoFS,
		Audit:   a,
		Limiter: lim,
	}
}

// forkDeps builds a fork.Deps. The fork package needs RepoFS to
// resolve the on-disk bare repo for ref updates.
func (h *Handlers) forkDeps() fork.Deps {
	a := h.d.Audit
	if a == nil {
		a = audit.NewRecorder()
	}
	return fork.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Audit:  a,
		Logger: h.d.Logger,
	}
}
