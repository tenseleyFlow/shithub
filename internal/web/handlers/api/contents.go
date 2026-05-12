// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountContents registers the S50 §12 repo-contents REST surface.
//
//	GET /api/v1/repos/{o}/{r}/contents/*[?ref=]
//
// One endpoint, two response shapes (mirrors GitHub):
//   - Path resolves to a tree → JSON array of entries.
//   - Path resolves to a blob → JSON object with base64-encoded content.
//
// `ref` defaults to the repo's default branch; accepts a branch name,
// tag, or commit SHA. Empty path (`/contents/`) returns the repo root.
//
// Scope: `repo:read`. Policy gate: `ActionRepoRead`.
func (h *Handlers) mountContents(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/contents/*", h.contentsGet)
		// `/contents` (no trailing slash) and `/contents/` (empty path)
		// both map to "repo root", so register both forms.
		r.Get("/api/v1/repos/{owner}/{repo}/contents", h.contentsGet)
	})
}

// contentBlobMaxBytes caps a single-file response. Anything larger
// trips the `truncated: true` flag and the content field comes back
// empty — clients should fall back to the raw download path
// (`/raw/...`) for the full bytes.
const contentBlobMaxBytes = 1 << 20 // 1 MiB

type contentEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "file" | "dir" | "symlink" | "submodule"
	Size      int64  `json:"size,omitempty"`
	SHA       string `json:"sha,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	IsBinary  bool   `json:"binary,omitempty"`
}

func (h *Handlers) contentsGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	path := strings.TrimPrefix(strings.TrimSpace(chi.URLParam(r, "*")), "/")
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}

	kind, _, size, err := git.StatPath(r.Context(), gitDir, ref, path)
	if err != nil {
		if errors.Is(err, git.ErrPathNotFound) {
			writeAPIError(w, http.StatusNotFound, "path not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: StatPath", "error", err, "ref", ref, "path", path)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	switch kind {
	case git.EntryTree:
		h.contentsTree(w, r, gitDir, ref, path)
		return
	case git.EntryBlob, git.EntrySymlink:
		h.contentsBlob(w, r, gitDir, ref, path, size)
		return
	case git.EntrySubmod:
		// GitHub returns a submodule entry shape; we surface the same
		// kind so the CLI can render "this is a submodule pointer".
		writeJSON(w, http.StatusOK, contentEntry{
			Path: path,
			Name: lastSegment(path),
			Type: "submodule",
		})
		return
	default:
		writeAPIError(w, http.StatusInternalServerError, "unknown content kind")
	}
}

func (h *Handlers) contentsTree(w http.ResponseWriter, r *http.Request, gitDir, ref, path string) {
	entries, err := git.LsTree(r.Context(), gitDir, ref, path)
	if err != nil {
		if errors.Is(err, git.ErrNotATree) {
			writeAPIError(w, http.StatusNotFound, "path not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: LsTree", "error", err, "ref", ref, "path", path)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]contentEntry, 0, len(entries))
	for _, e := range entries {
		full := e.Name
		if path != "" {
			full = path + "/" + e.Name
		}
		ce := contentEntry{
			Path: full,
			Name: e.Name,
			SHA:  e.OID,
		}
		switch e.Kind {
		case git.EntryTree:
			ce.Type = "dir"
		case git.EntryBlob:
			ce.Type = "file"
			ce.Size = e.Size
		case git.EntrySymlink:
			ce.Type = "symlink"
			ce.Size = e.Size
		case git.EntrySubmod:
			ce.Type = "submodule"
		}
		out = append(out, ce)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) contentsBlob(w http.ResponseWriter, r *http.Request, gitDir, ref, path string, size int64) {
	out := contentEntry{
		Path: path,
		Name: lastSegment(path),
		Type: "file",
		Size: size,
	}
	// Skip the body read for files over the cap — base64-inflating a
	// huge blob would balloon JSON without helping the caller. The CLI
	// is expected to fall through to the raw blob URL.
	if size > contentBlobMaxBytes {
		out.Truncated = true
		out.Encoding = "base64"
		writeJSON(w, http.StatusOK, out)
		return
	}
	body, err := git.ReadBlobBytes(r.Context(), gitDir, ref, path, contentBlobMaxBytes)
	if err != nil {
		if errors.Is(err, git.ErrBlobTooLarge) {
			out.Truncated = true
			out.Encoding = "base64"
			writeJSON(w, http.StatusOK, out)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: ReadBlobBytes", "error", err, "ref", ref, "path", path)
		writeAPIError(w, http.StatusInternalServerError, "read failed")
		return
	}
	out.Encoding = "base64"
	out.Content = base64.StdEncoding.EncodeToString(body)
	out.IsBinary = !utf8.Valid(body)
	writeJSON(w, http.StatusOK, out)
}

// lastSegment returns the basename of a slash-separated path. Empty
// path → empty string (the repo-root call).
func lastSegment(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
