// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/finder"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/highlight"
	mdrender "github.com/tenseleyFlow/shithub/internal/repos/markdown"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountCode registers the code-tab routes:
//
//	GET /{owner}/{repo}/tree/*
//	GET /{owner}/{repo}/blob/*
//	GET /{owner}/{repo}/raw/*
//	GET /{owner}/{repo}/find/*
//
// The leading {ref} segment is variable-length (refs may contain `/`).
// chi's `*` wildcard captures the rest; we resolve ref + path inside
// the handler against the repo's known ref list.
func (h *Handlers) MountCode(r chi.Router) {
	r.Get("/{owner}/{repo}/tree/*", h.codeTree)
	r.Get("/{owner}/{repo}/blob/*", h.codeBlob)
	r.Get("/{owner}/{repo}/raw/*", h.codeRaw)
	r.Get("/{owner}/{repo}/find/*", h.codeFinder)
}

// codeContext bundles the per-request data the code-tab handlers
// derive once at the top. Owner+repo come from chi; ref+path come from
// the wildcard, resolved against the repo's ref list.
type codeContext struct {
	owner    string
	row      reposdb.Repo
	gitDir   string
	refs     repogit.RefListing
	allRefs  []string
	ref      string // matched ref name (or 40-hex sha)
	subpath  string // path inside the ref, no leading slash
}

// loadCodeContext does the resolve dance for tree/blob/raw/find. On
// any failure it writes the response and returns ok=false.
func (h *Handlers) loadCodeContext(w http.ResponseWriter, r *http.Request) (*codeContext, bool) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return nil, false
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return nil, false
	}
	refs, err := repogit.ListRefs(r.Context(), gitDir)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "code: ListRefs", "error", err)
	}
	allNames := make([]string, 0, len(refs.Branches)+len(refs.Tags))
	for _, b := range refs.Branches {
		allNames = append(allNames, b.Name)
	}
	for _, t := range refs.Tags {
		allNames = append(allNames, t.Name)
	}

	rest := chi.URLParam(r, "*")
	rest = strings.Trim(rest, "/")
	segs := []string{}
	if rest != "" {
		segs = strings.Split(rest, "/")
	}
	if len(segs) == 0 {
		// no ref → default branch root tree
		ref := row.DefaultBranch
		return &codeContext{
			owner: owner.Username, row: row, gitDir: gitDir,
			refs: refs, allRefs: allNames, ref: ref, subpath: "",
		}, true
	}
	ref, sub, ok2 := repogit.ResolveRef(allNames, segs)
	if !ok2 {
		// Fallback: if the first segment looks like a hex sha, accept it.
		if len(segs[0]) == 40 && isHex(segs[0]) {
			ref = segs[0]
			sub = strings.Join(segs[1:], "/")
		} else {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return nil, false
		}
	}
	if !validateSubpath(sub) {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return nil, false
	}
	return &codeContext{
		owner: owner.Username, row: row, gitDir: gitDir,
		refs: refs, allRefs: allNames, ref: ref, subpath: sub,
	}, true
}

// codeTree renders the directory listing at <ref>:<subpath>. If the
// path turns out to be a blob, redirects to /blob/. README rendering
// for tree-roots is appended below the listing.
func (h *Handlers) codeTree(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	kind, _, _, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil {
		if errors.Is(err, repogit.ErrPathNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "code: StatPath", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if kind == repogit.EntryBlob {
		http.Redirect(w, r, "/"+cc.owner+"/"+cc.row.Name+"/blob/"+cc.ref+"/"+cc.subpath, http.StatusSeeOther)
		return
	}
	entries, err := repogit.LsTree(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil {
		if errors.Is(err, repogit.ErrNotATree) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "code: LsTree", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	// README detection on the requested directory only.
	readmeHTML := h.findAndRenderREADME(r, cc, entries)

	h.d.Render.RenderPage(w, r, "repo/tree", map[string]any{
		"Title":     cc.row.Name + " · " + cc.owner,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     cc.owner,
		"Repo":      cc.row,
		"Ref":       cc.ref,
		"Path":      cc.subpath,
		"Crumbs":    breadcrumbs(cc.owner, cc.row.Name, cc.ref, cc.subpath),
		"Entries":   entries,
		"Branches":  cc.refs.Branches,
		"Tags":      cc.refs.Tags,
		"README":    template.HTML(readmeHTML), //nolint:gosec // sanitized by mdrender
	})
}

// findAndRenderREADME looks for README* in the supplied entries (case-
// insensitive). Returns rendered HTML for markdown sources; returns a
// `<pre>`-wrapped escaped string for non-markdown text. Empty when
// no README is present.
func (h *Handlers) findAndRenderREADME(r *http.Request, cc *codeContext, entries []repogit.TreeEntry) string {
	const maxREADMEBytes = 1 * 1024 * 1024 // 1 MiB cap
	for _, e := range entries {
		if e.Kind != repogit.EntryBlob {
			continue
		}
		lower := strings.ToLower(e.Name)
		if !strings.HasPrefix(lower, "readme") {
			continue
		}
		full := joinPath(cc.subpath, e.Name)
		body, err := repogit.ReadBlobBytes(r.Context(), cc.gitDir, cc.ref, full, maxREADMEBytes)
		if err != nil && !errors.Is(err, repogit.ErrBlobTooLarge) {
			return ""
		}
		// Markdown: render via Goldmark + sanitizer.
		if hasExt(lower, []string{".md", ".markdown"}) {
			out, mderr := mdrender.RenderHTML(body)
			if mderr == nil {
				return out
			}
		}
		// Non-markdown plain text: escape + <pre>.
		return "<pre class=\"shithub-readme-plain\">" + template.HTMLEscapeString(string(body)) + "</pre>"
	}
	return ""
}

func hasExt(filename string, exts []string) bool {
	for _, e := range exts {
		if strings.HasSuffix(filename, e) {
			return true
		}
	}
	return false
}

// codeBlob renders the file viewer.
func (h *Handlers) codeBlob(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	kind, _, size, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil || kind != repogit.EntryBlob {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	const largeFileThreshold = 1 * 1024 * 1024 // 1 MiB
	const maxReadBytes = 4 * 1024 * 1024       // never read more than 4 MiB even for highlighting

	data := map[string]any{
		"Title":      cc.subpath + " · " + cc.row.Name,
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Owner":      cc.owner,
		"Repo":       cc.row,
		"Ref":        cc.ref,
		"Path":       cc.subpath,
		"Crumbs":     breadcrumbs(cc.owner, cc.row.Name, cc.ref, cc.subpath),
		"Branches":   cc.refs.Branches,
		"Tags":       cc.refs.Tags,
		"Size":       size,
		"IsLarge":    size > largeFileThreshold,
		"IsBinary":   false,
		"IsImage":    false,
		"IsMarkdown": false,
		"Language":   highlight.LanguageGuess(cc.subpath),
	}
	if size > largeFileThreshold {
		h.d.Render.RenderPage(w, r, "repo/blob", data)
		return
	}
	body, err := repogit.ReadBlobBytes(r.Context(), cc.gitDir, cc.ref, cc.subpath, maxReadBytes)
	if err != nil && !errors.Is(err, repogit.ErrBlobTooLarge) {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if isBinary(body) {
		data["IsBinary"] = true
		if isImageExt(cc.subpath) && size <= 5*1024*1024 {
			data["IsImage"] = true
		}
		h.d.Render.RenderPage(w, r, "repo/blob", data)
		return
	}
	// Text path: highlight or markdown-render.
	if hasExt(strings.ToLower(cc.subpath), []string{".md", ".markdown"}) {
		data["IsMarkdown"] = true
		rendered, mderr := mdrender.RenderHTML(body)
		if mderr == nil {
			data["MarkdownHTML"] = template.HTML(rendered) //nolint:gosec // sanitized
		}
		data["RawSource"] = string(body)
	}
	highlighted := highlight.Render(cc.subpath, string(body))
	data["HighlightedHTML"] = template.HTML(highlighted) //nolint:gosec // chroma + classes
	h.d.Render.RenderPage(w, r, "repo/blob", data)
}

// codeRaw streams the raw bytes. Force `attachment` for executable
// content types (HTML/SVG/JS/etc.) since shithub doesn't have a
// separate raw host.
func (h *Handlers) codeRaw(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	kind, _, size, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil || kind != repogit.EntryBlob {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	contentType, forceAttachment := rawContentType(cc.subpath)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if forceAttachment {
		w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(cc.subpath)+`"`)
	}
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	if err := repogit.StreamBlob(r.Context(), cc.gitDir, cc.ref, cc.subpath, w); err != nil {
		h.d.Logger.WarnContext(r.Context(), "code: stream raw", "error", err)
	}
}

// codeFinder serves /find/{ref} — full list pre-filtered by `q`.
func (h *Handlers) codeFinder(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	paths, err := repogit.ListAllPaths(r.Context(), cc.gitDir, cc.ref)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "code: ListAllPaths", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	q := r.URL.Query().Get("q")
	matches := finder.Filter(paths, q, 200)
	h.d.Render.RenderPage(w, r, "repo/finder", map[string]any{
		"Title":     "Find file · " + cc.row.Name,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     cc.owner,
		"Repo":      cc.row,
		"Ref":       cc.ref,
		"Query":     q,
		"Matches":   matches,
		"Branches":  cc.refs.Branches,
		"Tags":      cc.refs.Tags,
	})
}

// breadcrumbs returns the click-each-segment slice for the tree/blob
// header.
type Breadcrumb struct {
	Name string
	URL  string
}

func breadcrumbs(owner, repoName, ref, subpath string) []Breadcrumb {
	out := []Breadcrumb{
		{Name: repoName, URL: fmt.Sprintf("/%s/%s/tree/%s", owner, repoName, ref)},
	}
	if subpath == "" {
		return out
	}
	parts := strings.Split(subpath, "/")
	for i, p := range parts {
		out = append(out, Breadcrumb{
			Name: p,
			URL:  fmt.Sprintf("/%s/%s/tree/%s/%s", owner, repoName, ref, strings.Join(parts[:i+1], "/")),
		})
	}
	return out
}

// validateSubpath is the path-traversal guard. Reject `..`, control
// chars, leading slash, and `\`.
func validateSubpath(p string) bool {
	if p == "" {
		return true
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == ".." {
			return false
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7f {
				return false
			}
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// rawContentType maps an extension to (Content-Type, forceAttachment).
// Executable content types force `attachment` to defeat XSS via raw view
// (no separate raw.host yet).
func rawContentType(p string) (string, bool) {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".html", ".htm", ".xhtml", ".svg", ".js", ".mjs", ".wasm":
		return "text/plain; charset=utf-8", true
	case ".png":
		return "image/png", false
	case ".jpg", ".jpeg":
		return "image/jpeg", false
	case ".gif":
		return "image/gif", false
	case ".webp":
		return "image/webp", false
	case ".pdf":
		return "application/pdf", false
	case ".css":
		return "text/css; charset=utf-8", false
	case ".json":
		return "application/json; charset=utf-8", false
	case ".txt", ".md", ".markdown", ".yml", ".yaml", ".toml", ".log":
		return "text/plain; charset=utf-8", false
	default:
		// Sniff by inspecting the body would be ideal, but we already
		// stream — fall back to text/plain for safety.
		return "text/plain; charset=utf-8", false
	}
}

func isImageExt(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// isBinary scans the first 8 KiB for a NUL byte.
func isBinary(b []byte) bool {
	const window = 8192
	if len(b) > window {
		b = b[:window]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// joinPath joins two slash-separated paths, ignoring an empty parent.
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

// silence pgtype unused-import warning when the loadRepoAndAuthorize
// helper is in this file's package but defined elsewhere.
var _ = pgtype.Int8{}
