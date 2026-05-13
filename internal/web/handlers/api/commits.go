// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountCommits registers the S50 §11 commits REST surface.
//
//	GET /api/v1/repos/{o}/{r}/commits[?sha=&path=&author=&since=&until=&page=&per_page=]
//	GET /api/v1/repos/{o}/{r}/commits/{sha}
//
// Mirrors GitHub's `/repos/{o}/{r}/commits` family. Scope: `repo:read`.
// Reads `git log` directly through `internal/repos/git.Log` /
// `git.GetCommit` so the response stays in sync with the on-disk
// repository.
func (h *Handlers) mountCommits(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/commits", h.commitsList)
		r.Get("/api/v1/repos/{owner}/{repo}/commits/{sha}", h.commitGet)
	})
}

type commitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitListItem struct {
	SHA          string               `json:"sha"`
	ShortSHA     string               `json:"short_sha"`
	Subject      string               `json:"subject"`
	Author       commitAuthor         `json:"author"`
	Body         string               `json:"body,omitempty"`
	Verification verificationResponse `json:"verification"`
}

// verificationResponse mirrors gh's documented commit verification
// object. Empty/never-verified commits emit the same shape gh uses
// for unsigned commits:
//
//	{verified: false, reason: "unsigned", signature: null, payload: null, verified_at: null}
//
// `payload` is the bytes the signature was computed over (commit
// object body minus the gpgsig header). Surfaced as a string in gh's
// JSON; we follow suit. nil bytes → JSON null via the pointer trick.
type verificationResponse struct {
	Verified   bool    `json:"verified"`
	Reason     string  `json:"reason"`
	Signature  *string `json:"signature"`
	Payload    *string `json:"payload"`
	VerifiedAt *string `json:"verified_at"`
}

func presentVerification(v sigverify.View) verificationResponse {
	resp := verificationResponse{
		Verified: v.Verified,
		Reason:   string(v.Reason),
	}
	if v.Signature != "" {
		s := v.Signature
		resp.Signature = &s
	}
	if len(v.Payload) > 0 {
		s := string(v.Payload)
		resp.Payload = &s
	}
	if v.VerifiedAt != nil {
		s := v.VerifiedAt.UTC().Format(time.RFC3339)
		resp.VerifiedAt = &s
	}
	return resp
}

type commitFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
	Adds    int    `json:"additions"`
	Deletes int    `json:"deletions"`
	Binary  bool   `json:"binary"`
}

type commitDetail struct {
	commitListItem
	Committer commitAuthor `json:"committer"`
	Parents   []string     `json:"parents"`
	TreeSHA   string       `json:"tree_sha"`
	Files     []commitFile `json:"files"`
	Stats     commitStats  `json:"stats"`
}

type commitStats struct {
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
	Total     int `json:"total"`
}

func presentCommit(c git.Commit, v sigverify.View) commitListItem {
	return commitListItem{
		SHA:      c.OID,
		ShortSHA: c.ShortOID,
		Subject:  c.Subject,
		Body:     c.Body,
		Author: commitAuthor{
			Name:  c.AuthorName,
			Email: c.AuthorEmail,
			Date:  c.AuthorWhen.UTC().Format(time.RFC3339),
		},
		Verification: presentVerification(v),
	}
}

func (h *Handlers) commitsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}

	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := r.URL.Query()
	opts := git.LogOptions{
		Ref:      strings.TrimSpace(q.Get("sha")),
		Path:     strings.TrimSpace(q.Get("path")),
		Author:   strings.TrimSpace(q.Get("author")),
		MaxCount: perPage,
		Skip:     (page - 1) * perPage,
	}
	if opts.Ref == "" {
		opts.Ref = repo.DefaultBranch
	}
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "since must be RFC3339")
			return
		}
		opts.Since = t
	}
	if raw := strings.TrimSpace(q.Get("until")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "until must be RFC3339")
			return
		}
		opts.Until = t
	}

	commits, err := git.Log(r.Context(), gitDir, opts)
	if err != nil {
		// Most likely cause is an uninitialised / empty repo. Mirror
		// branches/tags: empty payload, no 5xx.
		h.d.Logger.WarnContext(r.Context(), "api: git.Log", "error", err, "repo_id", repo.ID)
		writeJSON(w, http.StatusOK, []commitListItem{})
		return
	}

	// Batch-load verification cache rows for the page's OIDs. Failure
	// here is non-fatal — we fall back to UnsignedView per row so the
	// payload still renders, just without verification metadata.
	oids := make([]string, len(commits))
	for i, c := range commits {
		oids[i] = c.OID
	}
	verifications, err := sigverify.LoadViewsForOIDs(r.Context(), h.d.Pool, repo.ID, oids)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "api: load verifications", "error", err, "repo_id", repo.ID)
		verifications = map[string]sigverify.View{}
	}

	out := make([]commitListItem, 0, len(commits))
	for _, c := range commits {
		out = append(out, presentCommit(c, sigverify.LookupView(verifications, c.OID)))
	}
	// We don't have a cheap O(1) total count from `git log` without a
	// second walk, so emit `next`/`prev` only when a follow-on page
	// might exist (i.e. the current page filled to perPage).
	hasMore := len(commits) == perPage
	link := apipage.Page{Current: page, PerPage: perPage, Total: -1, HasMore: hasMore}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) commitGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	sha := strings.TrimSpace(chi.URLParam(r, "sha"))
	if sha == "" {
		writeAPIError(w, http.StatusNotFound, "commit not found")
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	cd, err := git.GetCommit(r.Context(), gitDir, sha)
	if err != nil {
		if errors.Is(err, git.ErrCommitNotFound) {
			writeAPIError(w, http.StatusNotFound, "commit not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: GetCommit", "error", err, "sha", sha)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	view, vErr := sigverify.LoadView(r.Context(), h.d.Pool, repo.ID, cd.OID)
	if vErr != nil && !errors.Is(vErr, pgx.ErrNoRows) {
		h.d.Logger.WarnContext(r.Context(), "api: load verification", "error", vErr, "repo_id", repo.ID, "oid", cd.OID)
		view = sigverify.UnsignedView()
	}

	out := commitDetail{
		commitListItem: presentCommit(cd.Commit, view),
		Committer: commitAuthor{
			Name:  cd.CommitterName,
			Email: cd.CommitterEmail,
			Date:  cd.CommitterWhen.UTC().Format(time.RFC3339),
		},
		Parents: append([]string{}, cd.Parents...),
		TreeSHA: cd.TreeOID,
	}
	for _, f := range cd.Files {
		out.Files = append(out.Files, commitFile{
			Path:    f.Path,
			OldPath: f.OldPath,
			Status:  f.Status,
			Adds:    f.Insert,
			Deletes: f.Delete,
			Binary:  f.Binary,
		})
		out.Stats.Additions += f.Insert
		out.Stats.Deletions += f.Delete
	}
	out.Stats.Total = out.Stats.Additions + out.Stats.Deletions
	writeJSON(w, http.StatusOK, out)
}
