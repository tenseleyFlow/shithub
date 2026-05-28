// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	srch "github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

const globalDashboardLimit = 50

type globalNavHandler struct {
	render *render.Renderer
	logger *slog.Logger
	pool   *pgxpool.Pool
}

type globalNavTab struct {
	Key      string
	Label    string
	Icon     string
	Href     string
	Selected bool
	Count    int64
	Meta     string
}

type globalIssueCounts struct {
	Total  int64
	Open   int64
	Closed int64
}

type globalIssueRow struct {
	ID           int64
	Owner        string
	RepoName     string
	Number       int64
	Title        string
	State        string
	Kind         string
	AuthorName   string
	URL          string
	RepoURL      string
	UpdatedAt    time.Time
	CommentCount int64
}

type globalRepoRow struct {
	ID              int64
	Owner           string
	Name            string
	Description     string
	Visibility      string
	PrimaryLanguage string
	LicenseKey      string
	StarCount       int64
	ForkCount       int64
	WatcherCount    int64
	IsFork          bool
	UpdatedAt       time.Time
	URL             string
}

func (h globalNavHandler) RedirectIssues(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/issues/assigned", http.StatusSeeOther)
}

func (h globalNavHandler) ServeIssues(w http.ResponseWriter, r *http.Request) {
	view := chi.URLParam(r, "view")
	if !validIssueView(view) {
		http.Redirect(w, r, "/issues/assigned", http.StatusSeeOther)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	rawQuery := strings.TrimSpace(r.URL.Query().Get("q"))
	state := globalStateFromRequest(r, rawQuery)
	displayQuery := rawQuery
	if displayQuery == "" {
		displayQuery = defaultIssueQuery(view, state)
	}
	parsed := globalDashboardIssueQuery(displayQuery, state)

	items, counts, err := h.listIssues(r.Context(), viewer.PolicyActor(), viewer.ID, "issue", view, parsed)
	if err != nil {
		h.logError(r.Context(), "global issues list", err)
		h.render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	data := map[string]any{
		"Title":        "Issues",
		"Heading":      issueViewLabel(view),
		"Query":        displayQuery,
		"State":        state,
		"StateTabs":    stateTabs(r.URL.Path, rawQuery, state, counts),
		"Views":        issueViewTabs(view, rawQuery, state),
		"Issues":       items,
		"Counts":       counts,
		"ResultCount":  countForState(counts, state),
		"EmptyTitle":   "No issues matched this view",
		"EmptyMessage": "Try another filter or open a repository to create a new issue.",
		"NewHref":      "/issues/new",
	}
	if err := h.render.RenderPage(w, r, "dashboard/issues", data); err != nil {
		h.logError(r.Context(), "render global issues", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h globalNavHandler) ServeNewIssue(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	searchText := strings.TrimSpace(r.URL.Query().Get("q"))
	repos, total, err := h.listRepos(r.Context(), viewer.PolicyActor(), viewer.ID, "contributions", searchText, true)
	if err != nil {
		h.logError(r.Context(), "global new issue repos", err)
		h.render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	data := map[string]any{
		"Title":       "New issue",
		"Heading":     "New issue",
		"Query":       searchText,
		"Repos":       repos,
		"TotalCount":  total,
		"EmptyTitle":  "No repositories with issues enabled",
		"EmptyBody":   "Create a repository or choose one with issues enabled before opening an issue.",
		"ChooseIssue": true,
	}
	if err := h.render.RenderPage(w, r, "dashboard/new_issue", data); err != nil {
		h.logError(r.Context(), "render global new issue", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h globalNavHandler) ServePulls(w http.ResponseWriter, r *http.Request) {
	view := normalizePullView(r.URL.Query().Get("view"))
	viewer := middleware.CurrentUserFromContext(r.Context())
	rawQuery := strings.TrimSpace(r.URL.Query().Get("q"))
	state := globalStateFromRequest(r, rawQuery)
	displayQuery := rawQuery
	if displayQuery == "" {
		displayQuery = defaultPullQuery(view, state, viewer.Username)
	}
	parsed := globalDashboardIssueQuery(displayQuery, state)

	items, counts, err := h.listIssues(r.Context(), viewer.PolicyActor(), viewer.ID, "pr", view, parsed)
	if err != nil {
		h.logError(r.Context(), "global pulls list", err)
		h.render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	data := map[string]any{
		"Title":       "Pull requests",
		"Heading":     "Pull requests",
		"Query":       displayQuery,
		"State":       state,
		"StateTabs":   stateTabs("/pulls", rawQuery, state, counts),
		"Views":       pullViewTabs(view, rawQuery, state),
		"Pulls":       items,
		"Counts":      counts,
		"ResultCount": countForState(counts, state),
		"EmptyTitle":  "No pull requests matched this view",
	}
	if err := h.render.RenderPage(w, r, "dashboard/pulls", data); err != nil {
		h.logError(r.Context(), "render global pulls", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h globalNavHandler) ServeRepos(w http.ResponseWriter, r *http.Request) {
	view := normalizeRepoView(r.URL.Query().Get("view"))
	viewer := middleware.CurrentUserFromContext(r.Context())
	searchText := strings.TrimSpace(r.URL.Query().Get("q"))
	repos, total, err := h.listRepos(r.Context(), viewer.PolicyActor(), viewer.ID, view, searchText, false)
	if err != nil {
		h.logError(r.Context(), "global repos list", err)
		h.render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	data := map[string]any{
		"Title":       "Repositories",
		"Heading":     repoViewLabel(view),
		"Query":       searchText,
		"Views":       repoViewTabs(view, searchText),
		"Repos":       repos,
		"TotalCount":  total,
		"EmptyTitle":  "No repositories matched this view",
		"EmptyBody":   "Create a repository or adjust your filters.",
		"NewRepoHref": "/new",
	}
	if err := h.render.RenderPage(w, r, "dashboard/repos", data); err != nil {
		h.logError(r.Context(), "render global repos", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h globalNavHandler) listIssues(ctx context.Context, actor policy.Actor, viewerID int64, kind, view string, q srch.ParsedQuery) ([]globalIssueRow, globalIssueCounts, error) {
	if h.pool == nil {
		return nil, globalIssueCounts{}, fmt.Errorf("database unavailable")
	}

	countQuery := q
	countQuery.StateFilter = ""
	countWhere, countArgs := buildGlobalIssueWhere(actor, viewerID, kind, view, countQuery)
	countSQL := fmt.Sprintf(`
SELECT
  COUNT(*)::bigint,
  COUNT(*) FILTER (WHERE i.state = 'open')::bigint,
  COUNT(*) FILTER (WHERE i.state = 'closed')::bigint
FROM issues i
JOIN repos r ON r.id = i.repo_id
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
WHERE %s`, countWhere)
	var counts globalIssueCounts
	if err := h.pool.QueryRow(ctx, countSQL, countArgs...).Scan(&counts.Total, &counts.Open, &counts.Closed); err != nil {
		return nil, counts, err
	}

	where, args := buildGlobalIssueWhere(actor, viewerID, kind, view, q)
	limitPlaceholder := nextPlaceholder(&args, globalDashboardLimit)
	ownerExpr := globalRepoOwnerExpr()
	orderBy := globalIssueOrderBy(q.SortFilter)
	listSQL := fmt.Sprintf(`
SELECT
  i.id,
  %s AS owner,
  r.name::text AS repo_name,
  i.number,
  i.title,
  i.state::text AS state,
  i.kind::text AS kind,
  COALESCE(author.username::text, '') AS author_name,
  i.updated_at,
  (SELECT COUNT(*)::bigint FROM issue_comments ic WHERE ic.issue_id = i.id) AS comment_count
FROM issues i
JOIN repos r ON r.id = i.repo_id
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
LEFT JOIN users author ON author.id = i.author_user_id
WHERE %s
ORDER BY %s
LIMIT %s`, ownerExpr, where, orderBy, limitPlaceholder)

	rows, err := h.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, counts, err
	}
	defer rows.Close()

	var out []globalIssueRow
	for rows.Next() {
		var row globalIssueRow
		if err := rows.Scan(
			&row.ID,
			&row.Owner,
			&row.RepoName,
			&row.Number,
			&row.Title,
			&row.State,
			&row.Kind,
			&row.AuthorName,
			&row.UpdatedAt,
			&row.CommentCount,
		); err != nil {
			return nil, counts, err
		}
		row.RepoURL = "/" + row.Owner + "/" + row.RepoName
		threadPath := "issues"
		if row.Kind == "pr" {
			threadPath = "pulls"
		}
		row.URL = row.RepoURL + "/" + threadPath + "/" + strconv.FormatInt(row.Number, 10)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, counts, err
	}
	return out, counts, nil
}

func (h globalNavHandler) listRepos(ctx context.Context, actor policy.Actor, viewerID int64, view, searchText string, requireIssues bool) ([]globalRepoRow, int64, error) {
	if h.pool == nil {
		return nil, 0, fmt.Errorf("database unavailable")
	}
	where, args := buildGlobalRepoWhere(actor, viewerID, view, searchText, requireIssues)
	countSQL := fmt.Sprintf(`
SELECT COUNT(*)::bigint
FROM repos r
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
WHERE %s`, where)
	var total int64
	if err := h.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listWhere, listArgs := buildGlobalRepoWhere(actor, viewerID, view, searchText, requireIssues)
	limitPlaceholder := nextPlaceholder(&listArgs, globalDashboardLimit)
	ownerExpr := globalRepoOwnerExpr()
	listSQL := fmt.Sprintf(`
SELECT
  r.id,
  %s AS owner,
  r.name::text AS name,
  r.description,
  r.visibility::text AS visibility,
  COALESCE(r.primary_language, '') AS primary_language,
  COALESCE(r.license_key, '') AS license_key,
  r.star_count,
  r.fork_count,
  r.watcher_count,
  (r.fork_of_repo_id IS NOT NULL) AS is_fork,
  r.updated_at
FROM repos r
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
WHERE %s
ORDER BY r.updated_at DESC, r.id DESC
LIMIT %s`, ownerExpr, listWhere, limitPlaceholder)

	rows, err := h.pool.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()

	var out []globalRepoRow
	for rows.Next() {
		var row globalRepoRow
		if err := rows.Scan(
			&row.ID,
			&row.Owner,
			&row.Name,
			&row.Description,
			&row.Visibility,
			&row.PrimaryLanguage,
			&row.LicenseKey,
			&row.StarCount,
			&row.ForkCount,
			&row.WatcherCount,
			&row.IsFork,
			&row.UpdatedAt,
		); err != nil {
			return nil, total, err
		}
		row.URL = "/" + row.Owner + "/" + row.Name
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, total, err
	}
	return out, total, nil
}

func buildGlobalIssueWhere(actor policy.Actor, viewerID int64, kind, view string, q srch.ParsedQuery) (string, []any) {
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", 1)
	args := append([]any{}, visArgs...)
	clauses := []string{
		visClause,
		"((" + "r.owner_user_id IS NOT NULL AND owner_user.deleted_at IS NULL AND owner_user.suspended_at IS NULL" + ") OR (" + "r.owner_org_id IS NOT NULL AND owner_org.deleted_at IS NULL" + "))",
	}

	if q.KindFilter != "" && q.KindFilter != kind {
		clauses = append(clauses, "FALSE")
	}
	if kind == "issue" && (q.MergedStateFilter != "" || q.MergedFilter != nil || q.ReviewRequestedFilter != "") {
		clauses = append(clauses, "FALSE")
	}

	kindPlaceholder := nextPlaceholder(&args, kind)
	clauses = append(clauses, "i.kind = "+kindPlaceholder+"::issue_kind")
	if q.StateFilter != "" {
		clauses = append(clauses, "i.state = "+nextPlaceholder(&args, q.StateFilter)+"::issue_state")
	}
	searchText := globalIssueSearchTextFromParsed(q)
	if searchText != "" {
		pattern := "%" + strings.ToLower(searchText) + "%"
		p := nextPlaceholder(&args, pattern)
		ownerExpr := "LOWER(" + globalRepoOwnerExpr() + ")"
		clauses = append(clauses, "(LOWER(i.title) LIKE "+p+" OR LOWER(i.body) LIKE "+p+" OR LOWER(r.name::text) LIKE "+p+" OR "+ownerExpr+" LIKE "+p+")")
	}
	if q.RepoFilter != nil {
		clauses = append(clauses, "LOWER(r.name::text) = LOWER("+nextPlaceholder(&args, q.RepoFilter.Name)+")")
		owner := nextPlaceholder(&args, q.RepoFilter.Owner)
		clauses = append(clauses, "(LOWER(owner_user.username::text) = LOWER("+owner+") OR LOWER(owner_org.slug::text) = LOWER("+owner+"))")
	}
	if q.OwnerFilter != "" {
		owner := nextPlaceholder(&args, q.OwnerFilter)
		clauses = append(clauses, "(LOWER(owner_user.username::text) = LOWER("+owner+") OR LOWER(owner_org.slug::text) = LOWER("+owner+"))")
	}
	if q.AuthorFilter != "" {
		if username, ok := resolveGlobalIssueUserFilter(actor, q.AuthorFilter); ok {
			clauses = append(clauses, "i.author_user_id = (SELECT id FROM users WHERE username = "+nextPlaceholder(&args, username)+")")
		} else {
			clauses = append(clauses, "FALSE")
		}
	}
	if q.AssigneeFilter != "" {
		if username, ok := resolveGlobalIssueUserFilter(actor, q.AssigneeFilter); ok {
			user := nextPlaceholder(&args, username)
			clauses = append(clauses, "EXISTS (SELECT 1 FROM issue_assignees ia JOIN users au ON au.id = ia.user_id WHERE ia.issue_id = i.id AND au.username = "+user+")")
		} else {
			clauses = append(clauses, "FALSE")
		}
	}
	if q.AssigneeAnyFilter {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = i.id)")
	}
	if q.CommenterFilter != "" {
		if username, ok := resolveGlobalIssueUserFilter(actor, q.CommenterFilter); ok {
			user := nextPlaceholder(&args, username)
			clauses = append(clauses, "EXISTS (SELECT 1 FROM issue_comments ic JOIN users cu ON cu.id = ic.author_user_id WHERE ic.issue_id = i.id AND cu.username = "+user+")")
		} else {
			clauses = append(clauses, "FALSE")
		}
	}
	if q.MentionFilter != "" {
		if username, ok := resolveGlobalIssueUserFilter(actor, q.MentionFilter); ok {
			mention := nextPlaceholder(&args, "%@"+username+"%")
			clauses = append(clauses, "(i.title ILIKE "+mention+" OR i.body ILIKE "+mention+" OR EXISTS (SELECT 1 FROM issue_comments im WHERE im.issue_id = i.id AND im.body ILIKE "+mention+"))")
		} else {
			clauses = append(clauses, "FALSE")
		}
	}
	if len(q.InvolvesFilters) > 0 {
		involves := make([]string, 0, len(q.InvolvesFilters))
		for _, raw := range q.InvolvesFilters {
			username, ok := resolveGlobalIssueUserFilter(actor, raw)
			if !ok {
				continue
			}
			user := nextPlaceholder(&args, username)
			mention := nextPlaceholder(&args, "%@"+username+"%")
			involves = append(involves, "(i.author_user_id = (SELECT id FROM users WHERE username = "+user+") OR EXISTS (SELECT 1 FROM issue_assignees ia JOIN users au ON au.id = ia.user_id WHERE ia.issue_id = i.id AND au.username = "+user+") OR EXISTS (SELECT 1 FROM issue_comments ic JOIN users cu ON cu.id = ic.author_user_id WHERE ic.issue_id = i.id AND cu.username = "+user+") OR i.title ILIKE "+mention+" OR i.body ILIKE "+mention+" OR EXISTS (SELECT 1 FROM issue_comments im WHERE im.issue_id = i.id AND im.body ILIKE "+mention+"))")
		}
		if len(involves) == 0 {
			clauses = append(clauses, "FALSE")
		} else {
			clauses = append(clauses, "("+strings.Join(involves, " OR ")+")")
		}
	}
	if q.ReviewRequestedFilter != "" {
		if username, ok := resolveGlobalIssueUserFilter(actor, q.ReviewRequestedFilter); ok {
			user := nextPlaceholder(&args, username)
			clauses = append(clauses, "EXISTS (SELECT 1 FROM pr_review_requests prr JOIN users ru ON ru.id = prr.requested_user_id WHERE prr.pr_issue_id = i.id AND ru.username = "+user+" AND prr.dismissed_at IS NULL AND prr.satisfied_by_review_id IS NULL)")
		} else {
			clauses = append(clauses, "FALSE")
		}
	}
	for _, label := range q.LabelFilters {
		labelArg := nextPlaceholder(&args, label)
		clauses = append(clauses, "EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = i.id AND l.name = "+labelArg+")")
	}
	if q.MilestoneFilter != "" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM milestones m WHERE m.id = i.milestone_id AND m.title = "+nextPlaceholder(&args, q.MilestoneFilter)+")")
	}
	for _, missing := range q.MissingFilters {
		switch missing {
		case "label":
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id)")
		case "milestone":
			clauses = append(clauses, "i.milestone_id IS NULL")
		case "assignee":
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = i.id)")
		case "project":
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM repo_project_items rpi WHERE rpi.issue_id = i.id)")
		}
	}
	if q.LockedFilter != nil {
		if *q.LockedFilter {
			clauses = append(clauses, "i.locked")
		} else {
			clauses = append(clauses, "NOT i.locked")
		}
	}
	if q.VisibilityFilter != "" {
		clauses = append(clauses, "r.visibility = "+nextPlaceholder(&args, q.VisibilityFilter)+"::repo_visibility")
	}
	if q.ArchivedFilter != nil {
		if *q.ArchivedFilter {
			clauses = append(clauses, "r.is_archived")
		} else {
			clauses = append(clauses, "NOT r.is_archived")
		}
	}
	if q.ForkFilter != nil {
		if *q.ForkFilter {
			clauses = append(clauses, "r.fork_of_repo_id IS NOT NULL")
		} else {
			clauses = append(clauses, "r.fork_of_repo_id IS NULL")
		}
	}
	if q.LanguageFilter != "" {
		clauses = append(clauses, "LOWER(COALESCE(r.primary_language, '')) = LOWER("+nextPlaceholder(&args, q.LanguageFilter)+")")
	}
	for _, topic := range q.TopicFilters {
		topicArg := nextPlaceholder(&args, topic)
		clauses = append(clauses, "EXISTS (SELECT 1 FROM repo_topics rt WHERE rt.repo_id = r.id AND rt.topic = "+topicArg+")")
	}
	clauses = appendGlobalIssueDateRangeClauses(clauses, &args, "i.created_at", q.CreatedFilter)
	clauses = appendGlobalIssueDateRangeClauses(clauses, &args, "i.updated_at", q.UpdatedFilter)
	clauses = appendGlobalIssueDateRangeClauses(clauses, &args, "i.closed_at", q.ClosedFilter)
	clauses = appendGlobalIssueMergedDateRangeClauses(clauses, &args, q.MergedFilter)
	switch q.MergedStateFilter {
	case "merged":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM pull_requests pr WHERE pr.issue_id = i.id AND pr.merged_at IS NOT NULL)")
	case "unmerged":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM pull_requests pr WHERE pr.issue_id = i.id AND pr.merged_at IS NULL)")
	}

	uid := nextPlaceholder(&args, viewerID)
	threadKind := func() string {
		return nextPlaceholder(&args, kind) + "::notification_thread_kind"
	}
	switch view {
	case "assigned":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = i.id AND ia.user_id = "+uid+")")
	case "created":
		clauses = append(clauses, "i.author_user_id = "+uid)
	case "mentioned":
		tk := threadKind()
		clauses = append(clauses, "(EXISTS (SELECT 1 FROM notifications n WHERE n.recipient_user_id = "+uid+" AND n.thread_kind = "+tk+" AND n.thread_id = i.id AND n.reason = 'mention') OR EXISTS (SELECT 1 FROM notification_threads nt WHERE nt.recipient_user_id = "+uid+" AND nt.thread_kind = "+tk+" AND nt.thread_id = i.id AND nt.reason = 'mention'))")
	case "recent":
		tk := threadKind()
		clauses = append(clauses, "(i.author_user_id = "+uid+" OR EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = i.id AND ia.user_id = "+uid+") OR EXISTS (SELECT 1 FROM notification_threads nt WHERE nt.recipient_user_id = "+uid+" AND nt.thread_kind = "+tk+" AND nt.thread_id = i.id AND nt.subscribed = true))")
	case "review-requests":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM pr_review_requests prr WHERE prr.pr_issue_id = i.id AND prr.requested_user_id = "+uid+" AND prr.dismissed_at IS NULL AND prr.satisfied_by_review_id IS NULL)")
	default:
		clauses = append(clauses, "i.author_user_id = "+uid)
	}
	return strings.Join(clauses, " AND "), args
}

func appendGlobalIssueDateRangeClauses(clauses []string, args *[]any, column string, dr *srch.DateRange) []string {
	if dr == nil {
		return clauses
	}
	if dr.HasFrom {
		clauses = append(clauses, column+" >= "+nextPlaceholder(args, dr.From))
	}
	if dr.HasTo {
		clauses = append(clauses, column+" < "+nextPlaceholder(args, dr.To))
	}
	return clauses
}

func appendGlobalIssueMergedDateRangeClauses(clauses []string, args *[]any, dr *srch.DateRange) []string {
	if dr == nil {
		return clauses
	}
	subclauses := []string{"pr.issue_id = i.id"}
	if dr.HasFrom {
		subclauses = append(subclauses, "pr.merged_at >= "+nextPlaceholder(args, dr.From))
	}
	if dr.HasTo {
		subclauses = append(subclauses, "pr.merged_at < "+nextPlaceholder(args, dr.To))
	}
	return append(clauses, "EXISTS (SELECT 1 FROM pull_requests pr WHERE "+strings.Join(subclauses, " AND ")+")")
}

func buildGlobalRepoWhere(actor policy.Actor, viewerID int64, view, searchText string, requireIssues bool) (string, []any) {
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", 1)
	args := append([]any{}, visArgs...)
	clauses := []string{
		visClause,
		"((r.owner_user_id IS NOT NULL AND owner_user.deleted_at IS NULL AND owner_user.suspended_at IS NULL) OR (r.owner_org_id IS NOT NULL AND owner_org.deleted_at IS NULL))",
	}
	if requireIssues {
		clauses = append(clauses, "r.has_issues = true")
	}
	if searchText != "" {
		pattern := "%" + strings.ToLower(searchText) + "%"
		p := nextPlaceholder(&args, pattern)
		ownerExpr := "LOWER(" + globalRepoOwnerExpr() + ")"
		clauses = append(clauses, "(LOWER(r.name::text) LIKE "+p+" OR LOWER(r.description) LIKE "+p+" OR "+ownerExpr+" LIKE "+p+")")
	}
	uid := nextPlaceholder(&args, viewerID)
	contributionClause := "(r.owner_user_id = " + uid + " OR r.owner_org_id IN (SELECT org_id FROM org_members WHERE user_id = " + uid + ") OR EXISTS (SELECT 1 FROM repo_collaborators rc WHERE rc.repo_id = r.id AND rc.user_id = " + uid + "))"
	switch view {
	case "mine":
		clauses = append(clauses, "r.owner_user_id = "+uid)
	case "forks":
		clauses = append(clauses, "r.fork_of_repo_id IS NOT NULL", contributionClause)
	case "admin":
		clauses = append(clauses, "(r.owner_user_id = "+uid+" OR r.owner_org_id IN (SELECT org_id FROM org_members WHERE user_id = "+uid+" AND role = 'owner') OR EXISTS (SELECT 1 FROM repo_collaborators rc WHERE rc.repo_id = r.id AND rc.user_id = "+uid+" AND rc.role = 'admin'))")
	default:
		clauses = append(clauses, contributionClause)
	}
	return strings.Join(clauses, " AND "), args
}

func nextPlaceholder(args *[]any, v any) string {
	*args = append(*args, v)
	return "$" + strconv.Itoa(len(*args))
}

func globalRepoOwnerExpr() string {
	return "COALESCE(owner_user.username::text, owner_org.slug::text, '')"
}

func validIssueView(view string) bool {
	switch view {
	case "assigned", "created", "mentioned", "recent":
		return true
	default:
		return false
	}
}

func normalizePullView(view string) string {
	switch view {
	case "assigned", "mentioned", "review-requests":
		return view
	default:
		return "created"
	}
}

func normalizeRepoView(view string) string {
	switch view {
	case "mine", "forks", "admin":
		return view
	default:
		return "contributions"
	}
}

func globalStateFromRequest(r *http.Request, rawQuery string) string {
	state := normalizeGlobalState(r.URL.Query().Get("state"))
	lower := strings.ToLower(rawQuery)
	switch {
	case strings.Contains(lower, "state:closed"), strings.Contains(lower, "is:closed"):
		return "closed"
	case strings.Contains(lower, "state:open"), strings.Contains(lower, "is:open"):
		return "open"
	default:
		return state
	}
}

func normalizeGlobalState(raw string) string {
	switch raw {
	case "closed", "all":
		return raw
	default:
		return "open"
	}
}

func globalDashboardIssueQuery(raw string, state string) srch.ParsedQuery {
	q := srch.ParseQuery(raw)
	if state == "all" {
		q.StateFilter = ""
	} else if state != "" {
		q.StateFilter = state
	}
	return q
}

func globalIssueSearchText(raw string) string {
	return globalIssueSearchTextFromParsed(srch.ParseQuery(raw))
}

func globalIssueSearchTextFromParsed(q srch.ParsedQuery) string {
	parts := make([]string, 0, len(q.Terms))
	for _, term := range q.Terms {
		if term.Negated {
			continue
		}
		if term.Phrase {
			parts = append(parts, term.Value)
			continue
		}
		parts = append(parts, term.Value)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func globalIssueOrderBy(sort string) string {
	switch sort {
	case "comments-asc":
		return "comment_count ASC, i.updated_at DESC, i.id DESC"
	case "comments-desc":
		return "comment_count DESC, i.updated_at DESC, i.id DESC"
	case "created-asc":
		return "i.created_at ASC, i.id ASC"
	case "created-desc":
		return "i.created_at DESC, i.id DESC"
	case "updated-asc":
		return "i.updated_at ASC, i.id ASC"
	default:
		return "i.updated_at DESC, i.id DESC"
	}
}

func resolveGlobalIssueUserFilter(actor policy.Actor, raw string) (string, bool) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
	if value == "" {
		return "", false
	}
	if strings.EqualFold(value, "me") {
		if actor.IsAnonymous || actor.Username == "" {
			return "", false
		}
		return actor.Username, true
	}
	return value, true
}

func globalQueryWithoutStateOperators(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Fields(raw)
	keep := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.ToLower(strings.TrimSpace(part))
		switch {
		case token == "":
			continue
		case strings.HasPrefix(token, "state:"),
			token == "is:open",
			token == "is:closed":
			continue
		default:
			keep = append(keep, part)
		}
	}
	return strings.Join(keep, " ")
}

func issueViewLabel(view string) string {
	switch view {
	case "created":
		return "Created by me"
	case "mentioned":
		return "Mentioned"
	case "recent":
		return "Recent activity"
	default:
		return "Assigned to me"
	}
}

func repoViewLabel(view string) string {
	switch view {
	case "mine":
		return "My repositories"
	case "forks":
		return "My forks"
	case "admin":
		return "Admin access"
	default:
		return "My contributions"
	}
}

func defaultIssueQuery(view, state string) string {
	statePart := globalStateQueryPart(state)
	switch view {
	case "created":
		return "is:issue" + statePart + " archived:false author:@me sort:updated-desc"
	case "mentioned":
		return "is:issue" + statePart + " archived:false mentions:@me sort:updated-desc"
	case "recent":
		return "is:issue" + statePart + " archived:false involves:@me sort:updated-desc"
	default:
		return "is:issue" + statePart + " archived:false assignee:@me sort:updated-desc"
	}
}

func defaultPullQuery(view, state, username string) string {
	if username == "" {
		username = "@me"
	} else {
		username = "@" + username
	}
	statePart := globalStateQueryPart(state)
	switch view {
	case "assigned":
		return "is:pr" + statePart + " archived:false assignee:@me sort:updated-desc"
	case "mentioned":
		return "is:pr" + statePart + " archived:false mentions:@me sort:updated-desc"
	case "review-requests":
		return "is:pr" + statePart + " archived:false review-requested:@me sort:updated-desc"
	default:
		return "is:pr" + statePart + " archived:false author:" + username
	}
}

func globalStateQueryPart(state string) string {
	if state == "" || state == "all" {
		return ""
	}
	return " state:" + state
}

func issueViewTabs(active, query, state string) []globalNavTab {
	return []globalNavTab{
		{Key: "assigned", Label: "Assigned to me", Icon: "people", Href: dashboardHref("/issues/assigned", queryValues(query, state, "")), Selected: active == "assigned"},
		{Key: "created", Label: "Created by me", Icon: "smiley", Href: dashboardHref("/issues/created", queryValues(query, state, "")), Selected: active == "created"},
		{Key: "mentioned", Label: "Mentioned", Icon: "mention", Href: dashboardHref("/issues/mentioned", queryValues(query, state, "")), Selected: active == "mentioned"},
		{Key: "recent", Label: "Recent activity", Icon: "history", Href: dashboardHref("/issues/recent", queryValues(query, state, "")), Selected: active == "recent"},
	}
}

func pullViewTabs(active, query, state string) []globalNavTab {
	return []globalNavTab{
		{Key: "created", Label: "Created", Href: dashboardHref("/pulls", queryValues(query, state, "created")), Selected: active == "created"},
		{Key: "assigned", Label: "Assigned", Href: dashboardHref("/pulls", queryValues(query, state, "assigned")), Selected: active == "assigned"},
		{Key: "mentioned", Label: "Mentioned", Href: dashboardHref("/pulls", queryValues(query, state, "mentioned")), Selected: active == "mentioned"},
		{Key: "review-requests", Label: "Review requests", Href: dashboardHref("/pulls", queryValues(query, state, "review-requests")), Selected: active == "review-requests"},
	}
}

func repoViewTabs(active, query string) []globalNavTab {
	return []globalNavTab{
		{Key: "contributions", Label: "My contributions", Icon: "people", Href: dashboardHref("/repos", repoQueryValues(query, "contributions")), Selected: active == "contributions"},
		{Key: "mine", Label: "My repositories", Icon: "repo", Href: dashboardHref("/repos", repoQueryValues(query, "mine")), Selected: active == "mine"},
		{Key: "forks", Label: "My forks", Icon: "repo-forked", Href: dashboardHref("/repos", repoQueryValues(query, "forks")), Selected: active == "forks"},
		{Key: "admin", Label: "Admin access", Icon: "gear", Href: dashboardHref("/repos", repoQueryValues(query, "admin")), Selected: active == "admin"},
	}
}

func stateTabs(path, query, active string, counts globalIssueCounts) []globalNavTab {
	query = globalQueryWithoutStateOperators(query)
	return []globalNavTab{
		{Key: "open", Label: "Open", Icon: "issue-opened", Count: counts.Open, Href: dashboardHref(path, queryValues(query, "open", "")), Selected: active == "open"},
		{Key: "closed", Label: "Closed", Icon: "check", Count: counts.Closed, Href: dashboardHref(path, queryValues(query, "closed", "")), Selected: active == "closed"},
		{Key: "all", Label: "All", Icon: "list-unordered", Count: counts.Total, Href: dashboardHref(path, queryValues(query, "all", "")), Selected: active == "all"},
	}
}

func queryValues(query, state, view string) url.Values {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if state != "" && state != "open" {
		values.Set("state", state)
	}
	if view != "" && view != "created" {
		values.Set("view", view)
	}
	return values
}

func repoQueryValues(query, view string) url.Values {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if view != "" && view != "contributions" {
		values.Set("view", view)
	}
	return values
}

func dashboardHref(path string, values url.Values) string {
	if encoded := values.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

func countForState(counts globalIssueCounts, state string) int64 {
	switch state {
	case "closed":
		return counts.Closed
	case "all":
		return counts.Total
	default:
		return counts.Open
	}
}

func (h globalNavHandler) logError(ctx context.Context, msg string, err error) {
	if h.logger != nil {
		h.logger.ErrorContext(ctx, msg, "error", err)
	}
}
