// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountRefs registers the S20 branches, tags, and compare routes.
// Settings routes are wired separately (require lifecycle middleware).
func (h *Handlers) MountRefs(r chi.Router) {
	r.Get("/{owner}/{repo}/branches", h.branchesList)
	r.Get("/{owner}/{repo}/tags", h.tagsList)
	r.Get("/{owner}/{repo}/compare", h.compareView)
	// Compare uses `...` as the base/head separator (matches GitHub).
	// chi can't represent the literal `...` in a route param so we use
	// a wildcard and parse server-side.
	r.Get("/{owner}/{repo}/compare/*", h.compareView)
}

// branchesList renders the branches index. Computes ahead/behind
// against the default branch for each one. Filterable as
// active/stale/all via the `?filter=` query.
func (h *Handlers) branchesList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	refs, err := repogit.ListRefs(r.Context(), gitDir)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	rules, _ := h.rq.ListBranchProtectionRules(r.Context(), h.d.Pool, row.ID)

	defaultBranch := row.DefaultBranch
	// Resolve the default's OID once so the cached AheadBehind key is
	// OID-based (immutable across same-OID pushes; cache stays hot
	// when other branches advance).
	defaultOID := ""
	for _, b := range refs.Branches {
		if b.Name == defaultBranch {
			defaultOID = b.OID
			break
		}
	}
	rows := make([]branchRow, 0, len(refs.Branches))
	now := time.Now()
	for _, b := range refs.Branches {
		br := branchRow{Name: b.Name, OID: b.OID}
		if len(b.OID) >= 7 {
			br.ShortOID = b.OID[:7]
		}
		// HeadOf gives us subject + author time without an extra log call.
		if hc, found, herr := repogit.HeadOf(r.Context(), gitDir, b.Name); herr == nil && found {
			br.LastSubject = hc.Subject
			br.LastWhen = hc.AuthorWhen
			br.Stale = now.Sub(hc.AuthorWhen) > 90*24*time.Hour
		}
		if b.Name != defaultBranch && defaultOID != "" {
			res, _ := repogit.AheadBehindCached(r.Context(), gitDir, repogit.AheadBehindKey{
				RepoID: row.ID, BaseOID: defaultOID, HeadOID: b.OID,
			})
			br.Ahead = res.Ahead
			br.Behind = res.Behind
		}
		br.IsDefault = b.Name == defaultBranch
		br.Protected = isBranchProtected(rules, b.Name)
		rows = append(rows, br)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].IsDefault != rows[j].IsDefault {
			return rows[i].IsDefault
		}
		return rows[i].LastWhen.After(rows[j].LastWhen)
	})

	filter := r.URL.Query().Get("filter")
	if filter == "active" || filter == "stale" {
		want := filter == "stale"
		filtered := rows[:0]
		for _, br := range rows {
			if br.Stale == want {
				filtered = append(filtered, br)
			}
		}
		rows = filtered
	}

	h.d.Render.RenderPage(w, r, "repo/branches", map[string]any{
		"Title":         "Branches · " + row.Name,
		"CSRFToken":     middleware.CSRFTokenForRequest(r),
		"Owner":         owner.Username,
		"Repo":          row,
		"DefaultBranch": defaultBranch,
		"Rows":          rows,
		"Filter":        filter,
		"Branches":      refs.Branches,
		"Tags":          refs.Tags,
	})
}

// branchRow is the per-row shape rendered on the branches list.
// Lifted to package level so the filter step can re-slice without an
// awkward anonymous-type signature.
type branchRow struct {
	Name        string
	OID         string
	ShortOID    string
	Ahead       int
	Behind      int
	LastSubject string
	LastWhen    time.Time
	Protected   bool
	IsDefault   bool
	Stale       bool
}

// isBranchProtected reports whether `branch` matches any rule. Used
// to render the "Protected" badge on the branches list.
func isBranchProtected(rules []reposdb.BranchProtectionRule, branch string) bool {
	for _, r := range rules {
		if ok, err := filepath.Match(r.Pattern, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// tagsList renders the tags index. For each tag we render name, OID,
// commit subject, and author age. The "Releases" slot per tag is a
// placeholder until first-class releases ship.
func (h *Handlers) tagsList(w http.ResponseWriter, r *http.Request) {
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

	type tagRow struct {
		Name       string
		OID        string
		ShortOID   string
		Subject    string
		AuthorName string
		AuthorWhen time.Time
	}
	rows := make([]tagRow, 0, len(refs.Tags))
	for _, t := range refs.Tags {
		tr := tagRow{Name: t.Name, OID: t.OID}
		if len(t.OID) >= 7 {
			tr.ShortOID = t.OID[:7]
		}
		if hc, _, herr := repogit.HeadOf(r.Context(), gitDir, "tags/"+t.Name); herr == nil {
			tr.Subject = hc.Subject
			tr.AuthorName = hc.AuthorName
			tr.AuthorWhen = hc.AuthorWhen
		}
		rows = append(rows, tr)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].AuthorWhen.After(rows[j].AuthorWhen)
	})

	h.d.Render.RenderPage(w, r, "repo/tags", map[string]any{
		"Title":     "Tags · " + row.Name,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner.Username,
		"Repo":      row,
		"Rows":      rows,
		"Branches":  refs.Branches,
		"Tags":      refs.Tags,
	})
}

// compareView renders the compare-against-base preview: a commits
// list (head-side only) plus the three-dot diff. Path shape is
// `/{owner}/{repo}/compare/<base>...<head>`. Cross-repo (e.g.
// `<base>...<fork:branch>`) is shape-supported but the full
// fork-PR flow ships in S22+S27.
func (h *Handlers) compareView(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	rest := strings.Trim(chi.URLParam(r, "*"), "/")
	hasSelection := rest != ""
	base := row.DefaultBranch
	head := row.DefaultBranch
	if hasSelection {
		var found bool
		base, head, found = strings.Cut(rest, "...")
		if !found {
			// Two-dot shape — accept but treat as three-dot for the diff.
			base, head, found = strings.Cut(rest, "..")
			if !found {
				head = rest
				base = row.DefaultBranch
			}
		}
		if base == "" {
			base = row.DefaultBranch
		}
		if head == "" {
			head = row.DefaultBranch
		}
	}

	// Strip cross-repo "fork:branch" prefix for the local path; full
	// cross-repo lookup lands in S22+S27.
	base = stripCrossRepoPrefix(base)
	head = stripCrossRepoPrefix(head)

	state := h.buildCompareState(r, owner.Username, row, gitDir, base, head, hasSelection, compareMenuTargetCompare)
	h.d.Render.RenderPage(w, r, "repo/compare", mergePageData(
		h.repoPageChrome(r, owner.Username, row, "code"),
		map[string]any{
			"Title":        "Compare · " + row.Name,
			"UseCompareJS": true,
			"Compare":      state,
			"Base":         state.Base,
			"Head":         state.Head,
			"HasSelection": state.HasSelection,
			"SameRef":      state.SameRef,
			"NotFound":     state.NotFound,
			"CommitsErr":   state.CommitsErr,
			"NoCommits":    state.NoCommits,
			"Ahead":        state.Ahead,
			"Behind":       state.Behind,
			"Commits":      state.Commits,
			"DiffHTML":     state.DiffHTML,
			"Stats":        state.Stats,
			"MergeState":   state.MergeState,
			"CanOpenPull":  state.CanOpenPull,
			"PullNewHref":  state.PullNewHref,
			"BaseMenu":     state.BaseMenu,
			"HeadMenu":     state.HeadMenu,
			"Examples":     state.Examples,
		},
	))
}

// stripCrossRepoPrefix turns "fork:branch" into "branch". Local-only
// for now — see comment in compareView.
func stripCrossRepoPrefix(s string) string {
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		return s[idx+1:]
	}
	return s
}
