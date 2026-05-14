// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	diffparse "github.com/tenseleyFlow/shithub/internal/repos/diff/parse"
	diffrender "github.com/tenseleyFlow/shithub/internal/repos/diff/render"
	diffsource "github.com/tenseleyFlow/shithub/internal/repos/diff/source"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/identity"
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/httpcache"
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
	refs, _ := git.ListRefs(r.Context(), gitDir)
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
		if matched, _, ok := git.ResolveRef(allNames, segs); ok {
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
	sinceRaw := strings.TrimSpace(q.Get("since"))
	untilRaw := strings.TrimSpace(q.Get("until"))
	since := parseDateParam(sinceRaw)
	until := parseUntilDateParam(untilRaw)

	// F01 PR-2: ETag short-circuit. The cache key is (repo_id,
	// branch_head_oid, page); filtered views (path/author/since/until)
	// bypass the cache entirely because the same key would resolve
	// to different content across filter variants. Bot crawls hit
	// unfiltered pages — that's the optimization target.
	filtered := pathFilter != "" || authorFilter != "" || sinceRaw != "" || untilRaw != ""
	if !filtered {
		if headOID, oidErr := git.ResolveRefOID(r.Context(), gitDir, ref); oidErr == nil {
			etag := httpcache.ETag(row.ID, headOID, page)
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
			w.Header().Add("Vary", "Cookie")
			if httpcache.IfNoneMatch(r, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}

	commits, err := git.Log(r.Context(), gitDir, git.LogOptions{
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
		rows = append(rows, newCommitRow(c, resolver.Resolve(r.Context(), c.AuthorEmail)))
	}

	// Batch-load signature verification cache for the page. Failure
	// is non-fatal — the rows default to UnsignedView and the page
	// renders without badges.
	if len(rows) > 0 {
		oids := make([]string, len(rows))
		for i := range rows {
			oids[i] = rows[i].Commit.OID
		}
		verifications, vErr := sigverify.LoadViewsForOIDs(r.Context(), h.d.Pool, row.ID, oids)
		if vErr != nil {
			h.d.Logger.WarnContext(r.Context(), "commits: load verifications", "error", vErr, "repo_id", row.ID)
		} else {
			for i := range rows {
				rows[i].Verification = sigverify.LookupView(verifications, rows[i].Commit.OID)
			}
		}
	}

	filterValues := commitFilterValues(pathFilter, authorFilter, sinceRaw, untilRaw)
	olderHref := ""
	if len(commits) == perPage {
		v := cloneURLValues(filterValues)
		v.Set("page", strconv.Itoa(page+1))
		olderHref = commitsHref(owner.Username, row.Name, ref, v)
	}
	newerHref := ""
	if page > 1 {
		v := cloneURLValues(filterValues)
		v.Set("page", strconv.Itoa(page-1))
		newerHref = commitsHref(owner.Username, row.Name, ref, v)
	}
	pathClearHref := ""
	if pathFilter != "" {
		v := cloneURLValues(filterValues)
		v.Del("path")
		pathClearHref = commitsHref(owner.Username, row.Name, ref, v)
	}
	allAuthorsValues := cloneURLValues(filterValues)
	allAuthorsValues.Del("author")
	selectedDate := until
	if selectedDate.IsZero() {
		selectedDate = since
	}

	h.d.Render.RenderPage(w, r, "repo/commits", map[string]any{
		"Title":            "Commits · " + row.Name,
		"CSRFToken":        middleware.CSRFTokenForRequest(r),
		"Owner":            owner.Username,
		"Repo":             row,
		"RepoActions":      h.repoActions(r, row.ID),
		"RepoCounts":       h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":      h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav":     "code",
		"Ref":              ref,
		"PathFilter":       pathFilter,
		"PathClearHref":    pathClearHref,
		"Author":           authorFilter,
		"AuthorLabel":      commitAuthorSummary(rows, authorFilter),
		"AllAuthorsHref":   commitsHref(owner.Username, row.Name, ref, allAuthorsValues),
		"AuthorFilters":    commitAuthorFilters(owner.Username, row.Name, ref, filterValues, rows, authorFilter),
		"Since":            sinceRaw,
		"Until":            untilRaw,
		"DateLabel":        commitDateSummary(sinceRaw, untilRaw),
		"Calendar":         commitCalendar(owner.Username, row.Name, ref, filterValues, selectedDate, q.Get("calendar_month"), rows, time.Now()),
		"CommitGroups":     groupCommitRows(rows),
		"RefMenu":          commitRefMenu(owner.Username, row.Name, ref, row.DefaultBranch, refs),
		"Page":             page,
		"NewerHref":        newerHref,
		"OlderHref":        olderHref,
		"HasActiveFilters": filtered,
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
	detail, err := git.GetCommit(r.Context(), gitDir, sha)
	if err != nil {
		if errors.Is(err, git.ErrCommitNotFound) {
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

	// S19 diff render: source the patch from the SHA and inline-render.
	mode := diffrender.ModeUnified
	if r.URL.Query().Get("diff") == "split" {
		mode = diffrender.ModeSplit
	}
	hideWS := r.URL.Query().Get("w") == "1"
	patch, perr := diffsource.FromCommit(r.Context(), gitDir, detail.OID, diffsource.Options{
		IgnoreWhitespace: hideWS, FindRenames: true,
	})
	var diffHTML template.HTML
	if perr != nil {
		h.d.Logger.WarnContext(r.Context(), "commit: diff source", "error", perr)
	} else {
		parsed, perr2 := diffparse.ParseBytes(patch)
		if perr2 != nil {
			h.d.Logger.WarnContext(r.Context(), "commit: diff parse", "error", perr2)
		} else {
			diffHTML = template.HTML(diffrender.Diff(parsed, diffrender.Options{Mode: mode})) //nolint:gosec // escapes inside
		}
	}

	// Load signature verification cache row for this commit. Failure
	// is non-fatal — falls back to UnsignedView so the page renders
	// without a badge.
	verification, vErr := sigverify.LoadView(r.Context(), h.d.Pool, row.ID, detail.OID)
	if vErr != nil {
		// pgx.ErrNoRows is normal (commit hasn't been verified yet);
		// LoadView already returns UnsignedView for that case. Other
		// errors get logged but the page still renders.
		h.d.Logger.WarnContext(r.Context(), "commit: load verification", "error", vErr, "repo_id", row.ID, "sha", detail.OID)
	}

	h.d.Render.RenderPage(w, r, "repo/commit", map[string]any{
		"Title":        detail.Subject + " · " + row.Name,
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"Owner":        owner.Username,
		"Repo":         row,
		"Detail":       detail,
		"Author":       author,
		"Committer":    committer,
		"BodyHTML":     template.HTML(linkifyCommitBody(detail.Body)), //nolint:gosec // escaped inside
		"DiffHTML":     diffHTML,
		"DiffMode":     string(mode),
		"HideWS":       hideWS,
		"Verification": verification,
	})
}

// blameView renders blame for a file. Reuses codeContext so the
// branch dropdown + breadcrumbs match the tree/blob pages.
func (h *Handlers) blameView(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContext(w, r)
	if !ok {
		return
	}
	chunks, err := git.Blame(r.Context(), cc.gitDir, git.BlameOptions{
		Ref:  cc.ref,
		Path: cc.subpath,
	})
	tooLarge := errors.Is(err, git.ErrBlameTooLarge)
	notBlob := errors.Is(err, git.ErrBlameOnBinary)
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
	commits, err := git.Log(r.Context(), gitDir, git.LogOptions{
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
	Commit       git.Commit
	Author       identity.Resolved
	AuthorLabel  string
	AuthorHref   string
	Verification sigverify.View
}

func newCommitRow(c git.Commit, author identity.Resolved) commitRow {
	return commitRow{
		Commit:       c,
		Author:       author,
		AuthorLabel:  commitAuthorLabel(c, author),
		AuthorHref:   commitAuthorHref(author),
		Verification: sigverify.UnsignedView(),
	}
}

type commitGroup struct {
	Title string
	Rows  []commitRow
}

type commitAuthorFilter struct {
	Label         string
	Query         string
	Href          string
	Active        bool
	User          bool
	AvatarURL     string
	IdenticonSeed string
}

type commitRefOption struct {
	Name      string
	Href      string
	Active    bool
	IsDefault bool
}

type commitRefMenuView struct {
	Branches []commitRefOption
	Tags     []commitRefOption
}

type commitCalendarView struct {
	MonthLabel    string
	YearLabel     string
	PrevMonthHref string
	NextMonthHref string
	ClearHref     string
	TodayHref     string
	Weeks         [][]commitCalendarDay
}

type commitCalendarDay struct {
	Label      string
	Href       string
	InMonth    bool
	IsSelected bool
	IsToday    bool
}

type blameChunkRow struct {
	Chunk  git.BlameChunk
	Author identity.Resolved
}

func commitAuthorLabel(c git.Commit, author identity.Resolved) string {
	if author.User {
		if author.Username != "" {
			return author.Username
		}
		return author.DisplayName
	}
	if strings.TrimSpace(c.AuthorName) != "" {
		return strings.TrimSpace(c.AuthorName)
	}
	return strings.TrimSpace(c.AuthorEmail)
}

func commitAuthorHref(author identity.Resolved) string {
	if !author.User || author.Username == "" {
		return ""
	}
	return "/" + pathEscapeSegments(author.Username)
}

func commitAuthorQuery(row commitRow) string {
	if row.Author.User && row.Author.Username != "" {
		return row.Author.Username
	}
	if email := strings.TrimSpace(row.Commit.AuthorEmail); email != "" {
		return email
	}
	return strings.TrimSpace(row.Commit.AuthorName)
}

func groupCommitRows(rows []commitRow) []commitGroup {
	groups := make([]commitGroup, 0)
	last := ""
	for _, row := range rows {
		title := row.Commit.AuthorWhen.Local().Format("January 2, 2006")
		if title != last {
			groups = append(groups, commitGroup{Title: title})
			last = title
		}
		groups[len(groups)-1].Rows = append(groups[len(groups)-1].Rows, row)
	}
	return groups
}

func commitAuthorSummary(rows []commitRow, active string) string {
	active = strings.TrimSpace(active)
	if active == "" {
		return "All users"
	}
	for _, row := range rows {
		if strings.EqualFold(commitAuthorQuery(row), active) || strings.EqualFold(row.AuthorLabel, active) {
			return row.AuthorLabel
		}
	}
	return active
}

func commitAuthorFilters(owner, repoName, ref string, base url.Values, rows []commitRow, active string) []commitAuthorFilter {
	type candidate struct {
		filter commitAuthorFilter
		key    string
	}
	seen := make(map[string]struct{})
	candidates := make([]candidate, 0, len(rows))
	for _, row := range rows {
		query := commitAuthorQuery(row)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values := cloneURLValues(base)
		values.Set("author", query)
		values.Del("page")
		values.Del("calendar_month")
		candidates = append(candidates, candidate{
			key: key,
			filter: commitAuthorFilter{
				Label:         row.AuthorLabel,
				Query:         query,
				Href:          commitsHref(owner, repoName, ref, values),
				Active:        strings.EqualFold(active, query) || strings.EqualFold(active, row.AuthorLabel),
				User:          row.Author.User,
				AvatarURL:     row.Author.AvatarURL,
				IdenticonSeed: row.Author.IdenticonSeed,
			},
		})
	}
	if active != "" {
		key := strings.ToLower(active)
		if _, ok := seen[key]; !ok {
			values := cloneURLValues(base)
			values.Set("author", active)
			values.Del("page")
			values.Del("calendar_month")
			candidates = append(candidates, candidate{
				key: key,
				filter: commitAuthorFilter{
					Label:  active,
					Query:  active,
					Href:   commitsHref(owner, repoName, ref, values),
					Active: true,
				},
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].filter.Active != candidates[j].filter.Active {
			return candidates[i].filter.Active
		}
		return strings.ToLower(candidates[i].filter.Label) < strings.ToLower(candidates[j].filter.Label)
	})
	out := make([]commitAuthorFilter, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.filter)
	}
	return out
}

func commitDateSummary(sinceRaw, untilRaw string) string {
	sinceRaw = strings.TrimSpace(sinceRaw)
	untilRaw = strings.TrimSpace(untilRaw)
	switch {
	case sinceRaw == "" && untilRaw == "":
		return "All time"
	case sinceRaw != "" && untilRaw != "":
		return formatCommitFilterDate(sinceRaw) + " - " + formatCommitFilterDate(untilRaw)
	case sinceRaw != "":
		return "Since " + formatCommitFilterDate(sinceRaw)
	default:
		return "Until " + formatCommitFilterDate(untilRaw)
	}
}

func formatCommitFilterDate(raw string) string {
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return raw
	}
	return t.Format("Jan 2, 2006")
}

func commitRefMenu(owner, repoName, current, defaultBranch string, refs git.RefListing) commitRefMenuView {
	out := commitRefMenuView{
		Branches: make([]commitRefOption, 0, len(refs.Branches)),
		Tags:     make([]commitRefOption, 0, len(refs.Tags)),
	}
	for _, ref := range refs.Branches {
		out.Branches = append(out.Branches, commitRefOption{
			Name:      ref.Name,
			Href:      commitsHref(owner, repoName, ref.Name, nil),
			Active:    ref.Name == current,
			IsDefault: ref.Name == defaultBranch,
		})
	}
	for _, ref := range refs.Tags {
		out.Tags = append(out.Tags, commitRefOption{
			Name:   ref.Name,
			Href:   commitsHref(owner, repoName, ref.Name, nil),
			Active: ref.Name == current,
		})
	}
	return out
}

func commitCalendar(owner, repoName, ref string, base url.Values, selected time.Time, monthParam string, rows []commitRow, now time.Time) commitCalendarView {
	if now.IsZero() {
		now = time.Now()
	}
	anchor := selected
	if anchor.IsZero() && len(rows) > 0 {
		anchor = rows[0].Commit.AuthorWhen
	}
	if anchor.IsZero() {
		anchor = now
	}
	if t, err := time.Parse("2006-01", strings.TrimSpace(monthParam)); err == nil {
		anchor = t
	}
	loc := anchor.Location()
	if loc == nil {
		loc = time.Local
	}
	monthStart := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, loc)
	gridStart := monthStart.AddDate(0, 0, -int(monthStart.Weekday()))
	weeks := make([][]commitCalendarDay, 6)
	for week := 0; week < 6; week++ {
		weeks[week] = make([]commitCalendarDay, 7)
		for day := 0; day < 7; day++ {
			d := gridStart.AddDate(0, 0, week*7+day)
			values := cloneURLValues(base)
			values.Set("until", d.Format("2006-01-02"))
			values.Del("page")
			values.Del("calendar_month")
			weeks[week][day] = commitCalendarDay{
				Label:      strconv.Itoa(d.Day()),
				Href:       commitsHref(owner, repoName, ref, values),
				InMonth:    d.Month() == monthStart.Month(),
				IsSelected: sameCalendarDate(d, selected),
				IsToday:    sameCalendarDate(d, now),
			}
		}
	}

	prevValues := cloneURLValues(base)
	prevValues.Set("calendar_month", monthStart.AddDate(0, -1, 0).Format("2006-01"))
	prevValues.Del("page")
	nextValues := cloneURLValues(base)
	nextValues.Set("calendar_month", monthStart.AddDate(0, 1, 0).Format("2006-01"))
	nextValues.Del("page")
	clearValues := cloneURLValues(base)
	clearValues.Del("since")
	clearValues.Del("until")
	clearValues.Del("calendar_month")
	clearValues.Del("page")
	todayValues := cloneURLValues(base)
	todayValues.Set("until", now.Format("2006-01-02"))
	todayValues.Del("calendar_month")
	todayValues.Del("page")

	return commitCalendarView{
		MonthLabel:    monthStart.Format("January"),
		YearLabel:     monthStart.Format("2006"),
		PrevMonthHref: commitsHref(owner, repoName, ref, prevValues),
		NextMonthHref: commitsHref(owner, repoName, ref, nextValues),
		ClearHref:     commitsHref(owner, repoName, ref, clearValues),
		TodayHref:     commitsHref(owner, repoName, ref, todayValues),
		Weeks:         weeks,
	}
}

func sameCalendarDate(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	bb := b.In(a.Location())
	return a.Year() == bb.Year() && a.Month() == bb.Month() && a.Day() == bb.Day()
}

func commitFilterValues(pathFilter, authorFilter, sinceRaw, untilRaw string) url.Values {
	values := url.Values{}
	if pathFilter != "" {
		values.Set("path", pathFilter)
	}
	if authorFilter != "" {
		values.Set("author", authorFilter)
	}
	if sinceRaw != "" {
		values.Set("since", sinceRaw)
	}
	if untilRaw != "" {
		values.Set("until", untilRaw)
	}
	return values
}

func commitsHref(owner, repoName, ref string, values url.Values) string {
	path := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/commits/" + pathEscapeSegments(ref)
	if len(values) == 0 {
		return path
	}
	encoded := values.Encode()
	if encoded == "" {
		return path
	}
	return path + "?" + encoded
}

func cloneURLValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for k, vv := range values {
		out[k] = append([]string(nil), vv...)
	}
	return out
}

func pathEscapeSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
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

func parseUntilDateParam(s string) time.Time {
	t := parseDateParam(s)
	if t.IsZero() {
		return t
	}
	return t.Add(24*time.Hour - time.Second)
}

// linkifyCommitBody produces escaped + linkified HTML from a commit
// message body. Two transformations:
//  1. URL detection (http/https) → `<a href="...">URL</a>`
//  2. Issue refs (`#NNN` and `owner/repo#NNN`) → `<span data-ref="...">…</span>`
//     so the S21 issue layer can post-render-link them without
//     re-rendering the page.
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
