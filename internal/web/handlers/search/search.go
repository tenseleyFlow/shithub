// SPDX-License-Identifier: AGPL-3.0-or-later

// Package search wires the S28 web search surface. The full results
// page lives at GET /search; the nav quick dropdown lives at GET
// /search/quick.
package search

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	srch "github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the handler set.
type Deps struct {
	Logger *slog.Logger
	Render *render.Renderer
	Pool   *pgxpool.Pool
	// Limiter, when non-nil, gates /search per-(viewer or IP). Audit
	// 2026-05-10 H4: search renders amplify FTS cost 5×–6× per
	// request, so without a limiter a single client can hammer the
	// DB. Optional in tests; required in production wiring.
	Limiter *ratelimit.Limiter
	// BillingEnforce carries PRO07's per-feature enforcement flags.
	// PRO-EXT01-08b consults UserAdvancedCodeSearch to decide whether
	// a regex query by a Free viewer is honoured (report-only logs +
	// proceeds) or refused with the upgrade banner.
	BillingEnforce config.EnforceConfig
}

// Handlers is the registered handler set. Construct via New.
type Handlers struct {
	d         Deps
	tabsCache *tabsCache // nil-safe — Mount constructs it
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("search: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("search: nil Pool")
	}
	return &Handlers{d: d, tabsCache: newTabsCache()}, nil
}

// SearchRateLimitPolicy is the per-(viewer or IP) limit applied to
// /search and /search/quick. 60/min is generous for human use
// (typical browse rate is well under this) but cheap to defeat any
// query-rotation attack that bypasses the tab-count cache (audit
// 2026-05-10 H4+H5). Surfaced as a var so tests can tighten it.
var SearchRateLimitPolicy = ratelimit.Policy{
	Scope:  "search",
	Max:    60,
	Window: 1 * time.Minute,
}

// Mount registers /search and /search/quick. When d.Limiter is set,
// both routes go through the rate-limit middleware before reaching
// the handlers — protects the FTS path from query-rotation attacks
// that the tab-counts cache alone can't absorb.
func (h *Handlers) Mount(r chi.Router) {
	if h.d.Limiter != nil {
		r.Group(func(r chi.Router) {
			r.Use(h.d.Limiter.Middleware(SearchRateLimitPolicy, searchRateLimitKey))
			r.Get("/search", h.results)
			r.Get("/search/quick", h.quick)
		})
		return
	}
	r.Get("/search", h.results)
	r.Get("/search/quick", h.quick)
}

// searchRateLimitKey picks the per-request key. Authed users key
// on user_id (so an attacker can't bypass by hopping accounts they
// don't have); anonymous users key on the trusted client IP. We
// trust X-Forwarded-For only when middleware.RealIP has already
// vetted it, which it does at the global stack level.
func searchRateLimitKey(r *http.Request) string {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !viewer.IsAnonymous() {
		return "u:" + intString(int(viewer.ID))
	}
	if ip, ok := ratelimit.ClientIP(r, true); ok {
		return "ip:" + ip.String()
	}
	return ""
}

func (h *Handlers) deps() srch.Deps {
	return srch.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
}

// regexCodeSearchAllowed reports whether the viewer can run regex
// code search (PRO-EXT01-08b) and returns a UI affordance hint —
// "" when no affordance is needed (Pro user or anonymous on a path
// that doesn't surface the toggle), or a non-empty key when the
// toggle should render with the pro-lock variant. Report-only mode
// (the default) still allows the regex to run for a Free user; the
// affordance is shown regardless so the gate is discoverable.
func (h *Handlers) regexCodeSearchAllowed(ctx context.Context, actor policy.Actor) (bool, string) {
	if actor.IsAnonymous {
		// Anonymous viewers don't have a plan; suppress the affordance
		// to avoid suggesting they "upgrade" before they sign in.
		return false, "anonymous"
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(actor.UserID),
		entitlements.FeatureAdvancedCodeSearch)
	if err != nil {
		return false, "anonymous"
	}
	if decision.Allowed {
		return true, ""
	}
	// Free user. In enforce mode, refuse to run the regex; in report-
	// only mode, log the would-deny and run it anyway. The affordance
	// always shows so the gate is visible.
	if h.d.BillingEnforce.UserAdvancedCodeSearch {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(actor.UserID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", actor.UserID,
			"feature", string(entitlements.FeatureAdvancedCodeSearch),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "enforce",
			"surface", "regex_code_search")
		return false, "upgrade"
	}
	h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
		"principal", billing.PrincipalForUser(actor.UserID).String(),
		"principal_kind", string(billing.SubjectKindUser),
		"principal_id", actor.UserID,
		"feature", string(entitlements.FeatureAdvancedCodeSearch),
		"reason", string(decision.Reason),
		"required_plan", string(decision.RequiredPlan),
		"mode", "report_only",
		"surface", "regex_code_search")
	return true, "upgrade"
}

func (h *Handlers) actor(r *http.Request) policy.Actor {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		return policy.AnonymousActor()
	}
	return viewer.PolicyActor()
}

// results renders the full /search page with type tabs.
func (h *Handlers) results(w http.ResponseWriter, r *http.Request) {
	rawQ := r.URL.Query().Get("q")
	tab := normalizeSearchTab(r.URL.Query().Get("type"))
	regexMode := r.URL.Query().Get("regex") == "on"
	page := pageFromRequest(r)

	parsed := srch.ParseQuery(rawQ)
	actor := h.actor(r)
	deps := h.deps()
	regexAllowed, regexAffordance := h.regexCodeSearchAllowed(r.Context(), actor)

	data := map[string]any{
		"Title":               "Search",
		"Query":               rawQ,
		"GlobalSearchQuery":   rawQ,
		"Tab":                 tab,
		"Page":                page,
		"Parsed":              parsed,
		"PageSize":            srch.PageSize,
		"SearchProTip":        searchProTip(tab),
		"RegexMode":           regexMode,
		"RegexAllowed":        regexAllowed,
		"RegexFeatureKey":     string(entitlements.FeatureAdvancedCodeSearch),
		"RegexLockAffordance": regexAffordance,
	}

	if !parsed.HasContent() {
		data["EmptyQuery"] = true
		data["SearchTabs"] = h.searchTabs(r, actor, parsed, rawQ, tab)
		_ = h.d.Render.RenderPage(w, r, "search/results", data)
		return
	}

	offset := (page - 1) * srch.PageSize
	switch tab {
	case "repositories":
		rows, total, err := srch.SearchRepos(r.Context(), deps, actor, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search repos", "error", err)
		}
		data["Repos"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "issues":
		rows, total, err := srch.SearchIssues(r.Context(), deps, actor, parsed, "issue", srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search issues", "error", err)
		}
		data["Issues"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "pullrequests":
		rows, total, err := srch.SearchIssues(r.Context(), deps, actor, parsed, "pr", srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search pulls", "error", err)
		}
		data["Issues"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "users":
		rows, total, err := srch.SearchUsers(r.Context(), deps, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search users", "error", err)
		}
		data["Users"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	case "code":
		if regexMode && regexAllowed {
			rows, total, err := srch.SearchCodeRegex(r.Context(), deps, actor, srch.CodeRegexParams{
				RegexPattern: rawQ,
				RepoFilter:   parsed.RepoFilter,
			}, srch.PageSize, offset)
			if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
				if errors.Is(err, srch.ErrRegexInvalid) {
					data["RegexError"] = "Invalid regex pattern."
				} else {
					h.d.Logger.ErrorContext(r.Context(), "search code regex", "error", err)
				}
			}
			data["Code"] = rows
			data["Total"] = total
			data["HasNext"] = int64(page*srch.PageSize) < total
			break
		}
		if regexMode && !regexAllowed {
			// Free user attempted regex with enforce on (or no
			// entitlement at all). We surface the affordance — the
			// regular search still runs so the user gets *some*
			// result for their query string, but the regex toggle
			// is presented as a Pro feature.
			data["RegexBlocked"] = true
		}
		rows, total, err := srch.SearchCode(r.Context(), deps, actor, parsed, srch.PageSize, offset)
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(r.Context(), "search code", "error", err)
		}
		data["Code"] = rows
		data["Total"] = total
		data["HasNext"] = int64(page*srch.PageSize) < total
	default:
		// Unknown tab → render the page with the empty-state shape
		// rather than 400 (a typo in the URL shouldn't be a hard
		// error).
		data["EmptyQuery"] = true
	}
	data["HasPrev"] = page > 1
	data["SearchTabs"] = h.searchTabs(r, actor, parsed, rawQ, tab)
	data["ResultHeading"] = searchResultHeading(tab, data["Total"])
	if page > 1 {
		data["PrevHref"] = searchHref(rawQ, tab, page-1)
	}
	if next, ok := data["HasNext"].(bool); ok && next {
		data["NextHref"] = searchHref(rawQ, tab, page+1)
	}

	if err := h.d.Render.RenderPage(w, r, "search/results", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "search render", "error", err)
	}
}

func normalizeSearchTab(tab string) string {
	switch tab {
	case "", "repos", "repositories":
		return "repositories"
	case "code":
		return "code"
	case "issues":
		return "issues"
	case "pulls", "pullrequests":
		return "pullrequests"
	case "users":
		return "users"
	default:
		return "repositories"
	}
}

type searchTab struct {
	Key      string
	Label    string
	Icon     string
	Count    int64
	Href     string
	Selected bool
}

func (h *Handlers) searchTabs(r *http.Request, actor policy.Actor, parsed srch.ParsedQuery, rawQ, active string) []searchTab {
	tabs := []searchTab{
		{Key: "code", Label: "Code", Icon: "code"},
		{Key: "repositories", Label: "Repositories", Icon: "repo"},
		{Key: "issues", Label: "Issues", Icon: "issue-opened"},
		{Key: "pullrequests", Label: "Pull requests", Icon: "git-pull-request"},
		{Key: "users", Label: "Users", Icon: "people"},
	}
	for i := range tabs {
		tabs[i].Selected = tabs[i].Key == active
		tabs[i].Href = searchHref(rawQ, tabs[i].Key, 1)
	}
	if !parsed.HasContent() {
		return tabs
	}

	// Counts are cached per-(query, viewer) for tabsCacheTTL. The
	// active-tab's actual result rows are NOT cached here — only the
	// 5 count-only badge calls that pre-fix were the dominant cost
	// (audit 2026-05-10 H5). Single-flighted via lru.Group so a
	// thundering-herd on the same key doesn't spawn N waves.
	key := tabsCacheKey{q: canonicalizeQuery(parsed), userID: actorUserID(actor)}
	cached, err := h.tabsCache.g.Do(r.Context(), key, func(ctx context.Context) ([]searchTab, error) {
		return h.computeTabCounts(ctx, actor, parsed), nil
	})
	if err != nil {
		// Group.Do never caches errors and our fetch returns nil; this
		// path is unreachable today but kept for defensiveness.
		h.d.Logger.ErrorContext(r.Context(), "search tabs cache", "error", err)
		cached = h.computeTabCounts(r.Context(), actor, parsed)
	}
	// Merge cached counts into the freshly-built (Selected/Href-aware)
	// tabs slice. The cached value carries Counts and the same Key
	// ordering; everything else is per-request and not cached.
	for i := range tabs {
		for j := range cached {
			if cached[j].Key == tabs[i].Key {
				tabs[i].Count = cached[j].Count
				break
			}
		}
	}
	return tabs
}

// computeTabCounts is the cache miss path: 5 FTS count-only queries.
// Returned slice carries (Key, Count) only — Selected/Href/Label/
// Icon are per-request and applied by the caller.
func (h *Handlers) computeTabCounts(ctx context.Context, actor policy.Actor, parsed srch.ParsedQuery) []searchTab {
	deps := h.deps()
	out := []searchTab{
		{Key: "code"},
		{Key: "repositories"},
		{Key: "issues"},
		{Key: "pullrequests"},
		{Key: "users"},
	}
	for i := range out {
		var total int64
		var err error
		switch out[i].Key {
		case "repositories":
			_, total, err = srch.SearchRepos(ctx, deps, actor, parsed, 0, 0)
		case "code":
			_, total, err = srch.SearchCode(ctx, deps, actor, parsed, 0, 0)
		case "issues":
			_, total, err = srch.SearchIssues(ctx, deps, actor, parsed, "issue", 0, 0)
		case "pullrequests":
			_, total, err = srch.SearchIssues(ctx, deps, actor, parsed, "pr", 0, 0)
		case "users":
			_, total, err = srch.SearchUsers(ctx, deps, parsed, 0, 0)
		}
		if err != nil && !errors.Is(err, srch.ErrEmptyQuery) {
			h.d.Logger.ErrorContext(ctx, "search tab count", "tab", out[i].Key, "error", err)
			continue
		}
		out[i].Count = total
	}
	return out
}

// actorUserID returns 0 for anonymous, the user_id otherwise. Used
// as the (anon vs each-authed-user) discriminant in the tabs cache
// key — anonymous viewers all see the same public-only result set
// so they share a slot; authed viewers see private results based
// on their collab roles, so each gets their own.
func actorUserID(a policy.Actor) int64 {
	if a.IsAnonymous {
		return 0
	}
	return a.UserID
}

func searchHref(q, tab string, page int) string {
	v := url.Values{}
	v.Set("q", q)
	v.Set("type", tab)
	if page > 1 {
		v.Set("p", intString(page))
	}
	return "/search?" + v.Encode()
}

func searchResultHeading(tab string, total any) string {
	count, _ := total.(int64)
	switch tab {
	case "code":
		return plural(count, "code result", "code results")
	case "issues":
		return plural(count, "issue result", "issue results")
	case "pullrequests":
		return plural(count, "pull request result", "pull request results")
	case "users":
		return plural(count, "user result", "user results")
	default:
		return plural(count, "repository result", "repository results")
	}
}

func plural(count int64, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return int64String(count) + " " + many
}

func int64String(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func searchProTip(tab string) string {
	switch tab {
	case "issues", "pullrequests":
		return "Restrict your search to the title by using the in:title qualifier."
	case "code":
		return "Use repo:owner/name to limit code search to a single repository."
	case "users":
		return "Search by username or display name to find people faster."
	default:
		return "Press / to activate the search input again and adjust your query."
	}
}

// quick is the nav dropdown endpoint. Returns one fragment with
// the top N results across the implemented quick-search types.
func (h *Handlers) quick(w http.ResponseWriter, r *http.Request) {
	rawQ := r.URL.Query().Get("q")
	parsed := srch.ParseQuery(rawQ)
	if !parsed.HasContent() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	actor := h.actor(r)
	deps := h.deps()

	repos, _, _ := srch.SearchRepos(r.Context(), deps, actor, parsed, srch.QuickResultsLimit, 0)
	issues, _, _ := srch.SearchIssues(r.Context(), deps, actor, parsed, "", srch.QuickResultsLimit, 0)
	users, _, _ := srch.SearchUsers(r.Context(), deps, parsed, srch.QuickResultsLimit, 0)

	data := map[string]any{
		"Query":      rawQ,
		"SearchHref": searchHref(rawQ, "repositories", 1),
		"Repos":      repos,
		"Issues":     issues,
		"Users":      users,
	}
	if err := h.d.Render.RenderFragment(w, "search/quick_dropdown", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "quick render", "error", err)
	}
}

// pageFromRequest pulls ?page=N, defaulting to 1 on missing/invalid.
func pageFromRequest(r *http.Request) int {
	p := r.URL.Query().Get("p")
	if p == "" {
		p = r.URL.Query().Get("page")
	}
	if p == "" {
		return 1
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return 1
		}
		n = n*10 + int(c-'0')
		if n > 10000 {
			return 1
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
