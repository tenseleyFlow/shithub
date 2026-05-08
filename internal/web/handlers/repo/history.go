// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/identity"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountHistory registers the S18 routes:
//
//	GET /{owner}/{repo}/commits/{ref}/*       — list, with ?path= filter
//	GET /{owner}/{repo}/commits/{ref}.atom    — Atom feed
//	GET /{owner}/{repo}/commit/{sha}          — single commit
//	GET /{owner}/{repo}/blame/{ref}/{path...} — blame
//
// Mount BEFORE /tree/{ref}/* so the more-specific paths win chi's
// match-by-registration-order.
func (h *Handlers) MountHistory(r chi.Router) {
	// Atom is a literal-suffix match; chi can't combine wildcard with a
	// trailing literal, so we keep `{ref}.atom` for atom-only and use
	// `commits/*` for the HTML list (which resolves ref-with-slash).
	r.Get("/{owner}/{repo}/commits/{ref}.atom", h.commitsAtom)
	r.Get("/{owner}/{repo}/commits/*", h.commitsList)
	r.Get("/{owner}/{repo}/commit/{sha}", h.commitView)
	r.Get("/{owner}/{repo}/blame/*", h.blameView)
}

// commitsList renders the paginated commit history for a ref, with an
// optional `path` filter (set when the user clicks "history" on a blob
// or follows the deferred-from-S17 tree column).
func (h *Handlers) commitsList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	refs, _ := repogit.ListRefs(r.Context(), gitDir)
	allNames := make([]string, 0, len(refs.Branches)+len(refs.Tags))
	for _, b := range refs.Branches {
		allNames = append(allNames, b.Name)
	}
	for _, t := range refs.Tags {
		allNames = append(allNames, t.Name)
	}
	rest := strings.Trim(chi.URLParam(r, "*"), "/")
	ref := row.DefaultBranch
	if rest != "" {
		segs := strings.Split(rest, "/")
		if matched, _, ok := repogit.ResolveRef(allNames, segs); ok {
			ref = matched
		} else if len(segs[0]) == 40 && isHex(segs[0]) {
			ref = segs[0]
		} else {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
	}

	q := r.URL.Query()
	const perPage = 30
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pathFilter := strings.TrimSpace(q.Get("path"))
	authorFilter := strings.TrimSpace(q.Get("author"))
	since := parseDateParam(q.Get("since"))
	until := parseDateParam(q.Get("until"))

	commits, err := repogit.Log(r.Context(), gitDir, repogit.LogOptions{
		Ref:      ref,
		MaxCount: perPage,
		Skip:     (page - 1) * perPage,
		Path:     pathFilter,
		Author:   authorFilter,
		Since:    since,
		Until:    until,
		Follow:   pathFilter != "",
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "commits: Log", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	resolver := identity.New(h.d.Pool)
	rows := make([]commitRow, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, commitRow{Commit: c, Author: resolver.Resolve(r.Context(), c.AuthorEmail)})
	}

	h.d.Render.RenderPage(w, r, "repo/commits", map[string]any{
		"Title":      "Commits · " + row.Name,
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Owner":      owner.Username,
		"Repo":       row,
		"Ref":        ref,
		"PathFilter": pathFilter,
		"Author":     authorFilter,
		"Since":      q.Get("since"),
		"Until":      q.Get("until"),
		"Rows":       rows,
		"Page":       page,
		"NextPage":   page + 1,
		"PrevPage":   page - 1,
		"HasMore":    len(commits) == perPage,
		"Branches":   refs.Branches,
		"Tags":       refs.Tags,
	})
}

// commitView renders the single-commit page: subject + body, parents,
// committer, file-changed table. Per-file diff bodies are S19 — for
// now the file rows show stats only.
func (h *Handlers) commitView(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	sha := chi.URLParam(r, "sha")
	if !validateSHA(sha) {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	detail, err := repogit.GetCommit(r.Context(), gitDir, sha)
	if err != nil {
		if errors.Is(err, repogit.ErrCommitNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "commit: GetCommit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	resolver := identity.New(h.d.Pool)
	author := resolver.Resolve(r.Context(), detail.AuthorEmail)
	committer := resolver.Resolve(r.Context(), detail.CommitterEmail)

	h.d.Render.RenderPage(w, r, "repo/commit", map[string]any{
		"Title":     detail.Subject + " · " + row.Name,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner.Username,
		"Repo":      row,
		"Detail":    detail,
		"Author":    author,
		"Committer": committer,
		"BodyHTML":  template.HTML(linkifyCommitBody(detail.Body)), //nolint:gosec // escaped inside
	})
}

// blameView renders blame for a file. Reuses codeContext so the
// branch dropdown + breadcrumbs match the tree/blob pages.
func (h *Handlers) blameView(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	chunks, err := repogit.Blame(r.Context(), cc.gitDir, repogit.BlameOptions{
		Ref:  cc.ref,
		Path: cc.subpath,
	})
	tooLarge := errors.Is(err, repogit.ErrBlameTooLarge)
	notBlob := errors.Is(err, repogit.ErrBlameOnBinary)
	if err != nil && !tooLarge && !notBlob {
		h.d.Logger.WarnContext(r.Context(), "blame: Blame", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	resolver := identity.New(h.d.Pool)
	chunkRows := make([]blameChunkRow, 0, len(chunks))
	for _, c := range chunks {
		chunkRows = append(chunkRows, blameChunkRow{
			Chunk:  c,
			Author: resolver.Resolve(r.Context(), c.AuthorEmail),
		})
	}

	h.d.Render.RenderPage(w, r, "repo/blame", map[string]any{
		"Title":     "Blame · " + cc.subpath,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     cc.owner,
		"Repo":      cc.row,
		"Ref":       cc.ref,
		"Path":      cc.subpath,
		"Crumbs":    breadcrumbs(cc.owner, cc.row.Name, cc.ref, cc.subpath),
		"Chunks":    chunkRows,
		"TooLarge":  tooLarge,
		"NotBlob":   notBlob,
	})
}

// commitsAtom serves the lightweight Atom feed of recent commits.
func (h *Handlers) commitsAtom(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	ref := chi.URLParam(r, "ref")
	commits, err := repogit.Log(r.Context(), gitDir, repogit.LogOptions{
		Ref: ref, MaxCount: 50,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "atom: Log", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	writeAtom(w, owner.Username, row.Name, ref, commits)
}

// commitRow / blameChunkRow attach the resolved identity to the bare
// git data so templates can render avatars and profile links without
// re-running the resolver.
type commitRow struct {
	Commit git.Commit
	Author identity.Resolved
}

type blameChunkRow struct {
	Chunk  git.BlameChunk
	Author identity.Resolved
}

// validateSHA accepts 7..40 hex chars. Git resolves short SHAs when
// unambiguous; we cap at 40 (full).
func validateSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	return isHex(s)
}

// parseDateParam takes a YYYY-MM-DD param and returns a UTC time. Any
// parse error returns the zero time, which the Log helper treats as
// "no filter."
func parseDateParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// linkifyCommitBody produces escaped + linkified HTML from a commit
// message body. Two transformations:
//   1. URL detection (http/https) → `<a href="...">URL</a>`
//   2. Issue refs (`#NNN` and `owner/repo#NNN`) → `<span data-ref="...">…</span>`
//      so the S21 issue layer can post-render-link them without
//      re-rendering the page.
//
// The output is HTML-escaped at every entry point — the only raw HTML
// is the wrapper tags this function emits.
func linkifyCommitBody(body string) string {
	if body == "" {
		return ""
	}
	escaped := template.HTMLEscapeString(body)
	escaped = issueRefRE.ReplaceAllStringFunc(escaped, func(m string) string {
		return `<span data-ref="` + template.HTMLEscapeString(m) + `">` + m + `</span>`
	})
	escaped = urlRE.ReplaceAllStringFunc(escaped, func(m string) string {
		return `<a href="` + m + `" rel="nofollow noopener">` + m + `</a>`
	})
	// Preserve newlines as <br> for plaintext-style rendering.
	return strings.ReplaceAll(escaped, "\n", "<br>")
}

var (
	issueRefRE = regexp.MustCompile(`(?:[a-z0-9][a-z0-9-]*\/[a-z0-9._-]+)?#\d+`)
	urlRE      = regexp.MustCompile(`https?:\/\/[^\s<>"']+`)
)
