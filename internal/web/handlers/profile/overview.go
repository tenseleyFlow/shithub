// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/net/html"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	profileContribRepoLimit  = 500
	profileContribMaxPerRepo = 5000
	profileReadmeMaxBytes    = 1 * 1024 * 1024
	profileActivityMonths    = 1
	profileActivityIssueRows = 1000
)

type profileOrgBadge struct {
	Slug        string
	DisplayName string
	AvatarURL   string
}

type profileReadme struct {
	Owner string
	Repo  string
	Ref   string
	Path  string
	HTML  template.HTML
}

type contributionCalendar struct {
	Total                       int
	Period                      string
	Weeks                       []contributionWeek
	Years                       []contributionYear
	CurrentYear                 int
	SelectedYear                int
	MonthLabel                  string
	MonthCommitCount            int
	MonthRepoCount              int
	HasRepositoryData           bool
	IncludePrivateContributions bool
	Activity                    []profileActivityMonth
	HasMoreActivity             bool
}

type contributionYear struct {
	Year   int
	Href   string
	Active bool
}

type contributionWeek struct {
	MonthLabel string
	Days       []contributionDay
}

type contributionDay struct {
	Date       string
	Title      string
	Count      int
	Level      int
	IsFuture   bool
	IsInWindow bool
}

type profileActivityMonth struct {
	Label           string
	Items           []profileActivityItem
	InitiallyHidden bool
}

type profileActivityItem struct {
	Kind    string
	Icon    string
	Summary string
	Total   int
	Repos   []profileActivityRepo
}

type profileActivityRepo struct {
	FullName      string
	URL           string
	Count         int
	CountLabel    string
	Language      string
	LanguageColor template.CSS
	DateLabel     string
	StateCounts   []profileActivityStateCount
}

type profileActivityStateCount struct {
	Label string
	Class string
	Count int
}

type profileContributionRepo struct {
	Repo                  reposdb.Repo
	OwnerSlug             string
	AllowIdentityFallback bool
}

func (h *Handlers) visibleUserRepos(ctx context.Context, userID int64, viewer middleware.CurrentUser) []reposdb.Repo {
	rows, err := reposdb.New().ListReposForOwnerUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: list repos", "user_id", userID, "error", err)
		return nil
	}
	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}
	out := make([]reposdb.Repo, 0, len(rows))
	for _, repo := range rows {
		if policy.IsVisibleTo(ctx, deps, actor, policy.NewRepoRefFromRepo(repo)) {
			out = append(out, repo)
		}
	}
	return out
}

func (h *Handlers) profileOrganizations(ctx context.Context, userID int64) []profileOrgBadge {
	rows, err := orgsdb.New().ListOrgsForUser(ctx, h.d.Pool, userID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: list orgs", "user_id", userID, "error", err)
		return nil
	}
	out := make([]profileOrgBadge, 0, len(rows))
	for _, row := range rows {
		label := row.DisplayName
		if label == "" {
			label = row.Slug
		}
		out = append(out, profileOrgBadge{
			Slug:        row.Slug,
			DisplayName: label,
			AvatarURL:   "/avatars/" + url.PathEscape(row.Slug),
		})
	}
	return out
}

func (h *Handlers) profileReadme(ctx context.Context, user usersdb.User, viewer middleware.CurrentUser) (profileReadme, bool) {
	if h.d.RepoFS == nil {
		return profileReadme{}, false
	}
	repo, err := reposdb.New().GetRepoByOwnerUserAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		Name:        user.Username,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.WarnContext(ctx, "profile overview: load profile readme repo", "user_id", user.ID, "error", err)
		}
		return profileReadme{}, false
	}
	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	if !policy.IsVisibleTo(ctx, policy.Deps{Pool: h.d.Pool}, actor, policy.NewRepoRefFromRepo(repo)) {
		return profileReadme{}, false
	}
	gitDir, err := h.d.RepoFS.RepoPath(user.Username, repo.Name)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: readme repo path", "repo_id", repo.ID, "error", err)
		return profileReadme{}, false
	}
	entries, err := repogit.LsTree(ctx, gitDir, repo.DefaultBranch, "")
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: readme tree", "repo_id", repo.ID, "error", err)
		return profileReadme{}, false
	}
	for _, entry := range entries {
		if entry.Kind != repogit.EntryBlob || !strings.HasPrefix(strings.ToLower(entry.Name), "readme") {
			continue
		}
		body, err := repogit.ReadBlobBytes(ctx, gitDir, repo.DefaultBranch, entry.Name, profileReadmeMaxBytes)
		if err != nil {
			h.d.Logger.WarnContext(ctx, "profile overview: readme blob", "repo_id", repo.ID, "path", entry.Name, "error", err)
			return profileReadme{}, false
		}
		html := ""
		lower := strings.ToLower(entry.Name)
		if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
			rendered, err := mdrender.RenderDocumentHTML(body)
			if err != nil {
				h.d.Logger.WarnContext(ctx, "profile overview: render readme", "repo_id", repo.ID, "error", err)
				return profileReadme{}, false
			}
			html = rewriteProfileMarkdownRelativeURLs(
				rendered,
				profileCodeRouteBase(user.Username, repo.Name, "blob", repo.DefaultBranch, ""),
				profileCodeRouteBase(user.Username, repo.Name, "blob", repo.DefaultBranch, ""),
				profileCodeRouteBase(user.Username, repo.Name, "raw", repo.DefaultBranch, ""),
				profileCodeRouteBase(user.Username, repo.Name, "raw", repo.DefaultBranch, ""),
			)
		} else {
			html = "<pre class=\"shithub-readme-plain\">" + template.HTMLEscapeString(string(body)) + "</pre>"
		}
		return profileReadme{
			Owner: user.Username,
			Repo:  repo.Name,
			Ref:   repo.DefaultBranch,
			Path:  entry.Name,
			HTML:  template.HTML(html), //nolint:gosec // sanitized by markdown renderer or escaped above.
		}, true
	}
	return profileReadme{}, false
}

func (h *Handlers) contributionCalendar(ctx context.Context, user usersdb.User, viewer middleware.CurrentUser, query url.Values) contributionCalendar {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	currentYear := today.Year()
	selectedYear := selectedContributionYear(query, currentYear)
	windowStart := today.AddDate(-1, 0, 1)
	windowEndDay := today
	period := "in the last year"
	if selectedYear != currentYear {
		windowStart = time.Date(selectedYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		windowEndDay = time.Date(selectedYear, time.December, 31, 0, 0, 0, 0, time.UTC)
		period = "in " + strconv.Itoa(selectedYear)
	}
	gridStart, weekCount := contributionGrid(windowStart, windowEndDay)
	windowEnd := windowEndDay.Add(24 * time.Hour)
	activityMonth := time.Date(windowEndDay.Year(), windowEndDay.Month(), 1, 0, 0, 0, 0, time.UTC)
	repos := h.profileContributionRepos(ctx, user, viewer, user.IncludePrivateContributions)
	activity := newProfileActivityBuilder()

	counts := map[string]int{}
	reposWithMonthActivity := map[int64]struct{}{}
	if h.d.RepoFS != nil && len(repos) > 0 {
		emails := h.verifiedEmails(ctx, user.ID)
		for i, source := range repos {
			if i >= profileContribRepoLimit {
				break
			}
			gitDir, err := h.d.RepoFS.RepoPath(source.OwnerSlug, source.Repo.Name)
			if err != nil {
				continue
			}
			commits, err := repogit.Log(ctx, gitDir, repogit.LogOptions{
				Ref:      source.Repo.DefaultBranch,
				MaxCount: profileContribMaxPerRepo,
				Since:    windowStart,
				Until:    windowEnd,
			})
			if err != nil {
				continue
			}
			for _, commit := range commits {
				if !commitMatchesProfileUser(commit, emails, user, source.AllowIdentityFallback) {
					continue
				}
				day := time.Date(commit.AuthorWhen.UTC().Year(), commit.AuthorWhen.UTC().Month(), commit.AuthorWhen.UTC().Day(), 0, 0, 0, 0, time.UTC)
				if day.Before(windowStart) || day.After(windowEndDay) {
					continue
				}
				key := day.Format("2006-01-02")
				counts[key]++
				activity.addCommit(day, source)
				if day.Year() == activityMonth.Year() && day.Month() == activityMonth.Month() {
					reposWithMonthActivity[source.Repo.ID] = struct{}{}
				}
			}
		}
	}
	for _, source := range repos {
		created := source.Repo.CreatedAt.Time.UTC()
		if !source.Repo.CreatedAt.Valid || created.Before(windowStart) || !created.Before(windowEnd) {
			continue
		}
		if source.Repo.OwnerUserID.Int64 != user.ID || source.Repo.Visibility != reposdb.RepoVisibilityPublic {
			continue
		}
		activity.addCreatedRepo(created, source)
	}
	h.addProfileThreadActivity(ctx, user, viewer, windowStart, windowEnd, activity)

	weeks := make([]contributionWeek, 0, weekCount)
	total := 0
	monthCommitCount := 0
	for w := 0; w < weekCount; w++ {
		weekStart := gridStart.AddDate(0, 0, w*7)
		week := contributionWeek{
			MonthLabel: contributionWeekMonthLabel(weekStart, w == 0),
			Days:       make([]contributionDay, 0, 7),
		}
		for d := 0; d < 7; d++ {
			day := weekStart.AddDate(0, 0, d)
			key := day.Format("2006-01-02")
			count := counts[key]
			inWindow := !day.Before(windowStart) && !day.After(windowEndDay)
			if inWindow {
				total += count
				if day.Year() == activityMonth.Year() && day.Month() == activityMonth.Month() {
					monthCommitCount += count
				}
			}
			week.Days = append(week.Days, contributionDay{
				Date:       key,
				Title:      contributionDayTitle(count, day),
				Count:      count,
				Level:      contributionLevel(count),
				IsFuture:   day.After(today),
				IsInWindow: inWindow,
			})
		}
		weeks = append(weeks, week)
	}
	return contributionCalendar{
		Total:                       total,
		Period:                      period,
		Weeks:                       weeks,
		Years:                       contributionYears(user.Username, currentYear, selectedYear),
		CurrentYear:                 currentYear,
		SelectedYear:                selectedYear,
		MonthLabel:                  activityMonth.Format("January 2006"),
		MonthCommitCount:            monthCommitCount,
		MonthRepoCount:              len(reposWithMonthActivity),
		HasRepositoryData:           h.d.RepoFS != nil && len(repos) > 0,
		IncludePrivateContributions: user.IncludePrivateContributions,
		Activity:                    activity.months(profileActivityMonths),
		HasMoreActivity:             activity.hasMore(profileActivityMonths),
	}
}

type profileActivityEvent struct {
	Kind      string
	When      time.Time
	FullName  string
	URL       string
	Language  string
	IsPrivate bool
	State     string
}

type profileActivityBuilder struct {
	events []profileActivityEvent
}

func newProfileActivityBuilder() *profileActivityBuilder {
	return &profileActivityBuilder{events: make([]profileActivityEvent, 0, 64)}
}

func (b *profileActivityBuilder) addCommit(day time.Time, source profileContributionRepo) {
	isPrivate := source.Repo.Visibility == reposdb.RepoVisibilityPrivate
	fullName := source.OwnerSlug + "/" + source.Repo.Name
	url := "/" + url.PathEscape(source.OwnerSlug) + "/" + url.PathEscape(source.Repo.Name)
	if isPrivate {
		fullName = "Private repositories"
		url = ""
	}
	b.events = append(b.events, profileActivityEvent{
		Kind:      "commits",
		When:      day,
		FullName:  fullName,
		URL:       url,
		Language:  pgTextStringOrEmpty(source.Repo.PrimaryLanguage),
		IsPrivate: isPrivate,
	})
}

func (b *profileActivityBuilder) addCreatedRepo(day time.Time, source profileContributionRepo) {
	b.events = append(b.events, profileActivityEvent{
		Kind:     "repos",
		When:     day,
		FullName: source.OwnerSlug + "/" + source.Repo.Name,
		URL:      "/" + url.PathEscape(source.OwnerSlug) + "/" + url.PathEscape(source.Repo.Name),
		Language: pgTextStringOrEmpty(source.Repo.PrimaryLanguage),
	})
}

func (b *profileActivityBuilder) addThread(day time.Time, kind, ownerSlug, repoName, state string) {
	b.events = append(b.events, profileActivityEvent{
		Kind:     kind,
		When:     day,
		FullName: ownerSlug + "/" + repoName,
		URL:      "/" + url.PathEscape(ownerSlug) + "/" + url.PathEscape(repoName),
		State:    state,
	})
}

func (b *profileActivityBuilder) hasMore(initialMonths int) bool {
	return len(b.monthKeys()) > initialMonths
}

func (b *profileActivityBuilder) months(initialMonths int) []profileActivityMonth {
	keys := b.monthKeys()
	byMonth := map[string]map[string]map[string]*profileActivityRepo{}
	monthTimes := map[string]time.Time{}
	for _, event := range b.events {
		month := time.Date(event.When.UTC().Year(), event.When.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		key := month.Format("2006-01")
		monthTimes[key] = month
		if byMonth[key] == nil {
			byMonth[key] = map[string]map[string]*profileActivityRepo{}
		}
		if byMonth[key][event.Kind] == nil {
			byMonth[key][event.Kind] = map[string]*profileActivityRepo{}
		}
		repoKey := event.FullName
		if event.IsPrivate {
			repoKey = "__private__"
		}
		repo := byMonth[key][event.Kind][repoKey]
		if repo == nil {
			language := event.Language
			if event.IsPrivate {
				language = ""
			}
			repo = &profileActivityRepo{
				FullName:      event.FullName,
				URL:           event.URL,
				Language:      language,
				LanguageColor: template.CSS(orgLanguageColor(language)), //nolint:gosec // server-side constant map.
			}
			byMonth[key][event.Kind][repoKey] = repo
		}
		repo.Count++
		if event.Kind == "repos" {
			repo.DateLabel = event.When.UTC().Format("Jan 2")
		}
		if event.State != "" {
			incrementActivityState(repo, event.State)
		}
	}

	out := make([]profileActivityMonth, 0, len(keys))
	for i, key := range keys {
		kindRepos := byMonth[key]
		items := make([]profileActivityItem, 0, len(kindRepos))
		for _, kind := range []string{"commits", "repos", "pulls", "issues"} {
			repos, ok := kindRepos[kind]
			if !ok {
				continue
			}
			item := profileActivityItem{
				Kind:  kind,
				Icon:  profileActivityIcon(kind),
				Repos: sortedActivityRepos(kind, repos),
			}
			for _, repo := range repos {
				item.Total += repo.Count
			}
			item.Summary = profileActivitySummary(kind, item.Total, len(repos))
			items = append(items, item)
		}
		out = append(out, profileActivityMonth{
			Label:           monthTimes[key].Format("January 2006"),
			Items:           items,
			InitiallyHidden: i >= initialMonths,
		})
	}
	return out
}

func (b *profileActivityBuilder) monthKeys() []string {
	seen := map[string]struct{}{}
	for _, event := range b.events {
		key := event.When.UTC().Format("2006-01")
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys
}

func incrementActivityState(repo *profileActivityRepo, state string) {
	for i := range repo.StateCounts {
		if repo.StateCounts[i].Class == state {
			repo.StateCounts[i].Count++
			return
		}
	}
	repo.StateCounts = append(repo.StateCounts, profileActivityStateCount{
		Label: state,
		Class: state,
		Count: 1,
	})
}

func sortedActivityRepos(kind string, repos map[string]*profileActivityRepo) []profileActivityRepo {
	out := make([]profileActivityRepo, 0, len(repos))
	for _, repo := range repos {
		if kind == "commits" {
			repo.CountLabel = pluralizeCount(repo.Count, "commit", "commits")
		}
		sort.SliceStable(repo.StateCounts, func(i, j int) bool {
			return activityStateOrder(repo.StateCounts[i].Class) < activityStateOrder(repo.StateCounts[j].Class)
		})
		out = append(out, *repo)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if kind == "repos" && out[i].DateLabel != out[j].DateLabel {
			return out[i].DateLabel > out[j].DateLabel
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].FullName < out[j].FullName
	})
	if len(out) > 5 {
		return out[:5]
	}
	return out
}

func activityStateOrder(state string) int {
	switch state {
	case "merged":
		return 0
	case "open":
		return 1
	case "closed":
		return 2
	default:
		return 3
	}
}

func profileActivityIcon(kind string) string {
	switch kind {
	case "commits":
		return "git-commit"
	case "pulls":
		return "git-pull-request"
	case "issues":
		return "issue-opened"
	default:
		return "repo"
	}
}

func profileActivitySummary(kind string, total, repoCount int) string {
	switch kind {
	case "commits":
		return "Created " + pluralizeCount(total, "commit", "commits") + " in " + pluralizeCount(repoCount, "repository", "repositories")
	case "repos":
		return "Created " + pluralizeCount(total, "repository", "repositories")
	case "pulls":
		return "Opened " + pluralizeCount(total, "pull request", "pull requests") + " in " + pluralizeCount(repoCount, "repository", "repositories")
	case "issues":
		return "Opened " + pluralizeCount(total, "issue", "issues") + " in " + pluralizeCount(repoCount, "repository", "repositories")
	default:
		return pluralizeCount(total, "contribution", "contributions")
	}
}

func pluralizeCount(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return strconv.Itoa(count) + " " + many
}

func (h *Handlers) addProfileThreadActivity(ctx context.Context, user usersdb.User, viewer middleware.CurrentUser, windowStart, windowEnd time.Time, activity *profileActivityBuilder) {
	rows, err := issuesdb.New().ListProfileAuthoredIssuesForUser(ctx, h.d.Pool, issuesdb.ListProfileAuthoredIssuesForUserParams{
		AuthorUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		CreatedAt:    pgtype.Timestamptz{Time: windowStart, Valid: true},
		CreatedAt_2:  pgtype.Timestamptz{Time: windowEnd, Valid: true},
		Limit:        profileActivityIssueRows,
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: list authored issues", "user_id", user.ID, "error", err)
		return
	}
	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}
	for _, row := range rows {
		if row.Visibility == issuesdb.RepoVisibilityPrivate && !user.IncludePrivateContributions {
			continue
		}
		ref := policy.RepoRef{
			ID:          row.RepoID,
			OwnerUserID: row.OwnerUserID.Int64,
			OwnerOrgID:  row.OwnerOrgID.Int64,
			Visibility:  string(row.Visibility),
		}
		if !policy.IsVisibleTo(ctx, deps, actor, ref) {
			continue
		}
		kind := "issues"
		state := "open"
		if row.State == issuesdb.IssueStateClosed {
			state = "closed"
		}
		if row.Kind == issuesdb.IssueKindPr {
			kind = "pulls"
			if row.MergedAt.Valid {
				state = "merged"
			}
		}
		activity.addThread(row.CreatedAt.Time, kind, row.OwnerSlug, row.RepoName, state)
	}
}

func (h *Handlers) profileContributionRepos(ctx context.Context, user usersdb.User, viewer middleware.CurrentUser, includePrivate bool) []profileContributionRepo {
	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}
	queries := reposdb.New()
	seen := map[int64]struct{}{}
	out := make([]profileContributionRepo, 0, 64)
	add := func(ownerSlug string, repo reposdb.Repo, allowIdentityFallback, affiliated bool) {
		if ownerSlug == "" {
			return
		}
		if _, ok := seen[repo.ID]; ok {
			return
		}
		repoRef := policy.NewRepoRefFromRepo(repo)
		if repoRef.IsPrivate() {
			if !includePrivate || !affiliated {
				return
			}
		} else {
			if !policy.IsVisibleTo(ctx, deps, actor, repoRef) {
				return
			}
		}
		seen[repo.ID] = struct{}{}
		out = append(out, profileContributionRepo{
			Repo:                  repo,
			OwnerSlug:             ownerSlug,
			AllowIdentityFallback: allowIdentityFallback,
		})
	}

	userRepos, err := queries.ListReposForOwnerUser(ctx, h.d.Pool, pgtype.Int8{Int64: user.ID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: contribution user repos", "user_id", user.ID, "error", err)
	} else {
		for _, repo := range userRepos {
			add(user.Username, repo, true, true)
		}
	}

	orgRows, err := orgsdb.New().ListOrgsForUser(ctx, h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: contribution orgs", "user_id", user.ID, "error", err)
	} else {
		for _, org := range orgRows {
			orgRepos, err := queries.ListReposForOwnerOrg(ctx, h.d.Pool, pgtype.Int8{Int64: org.OrgID, Valid: true})
			if err != nil {
				h.d.Logger.WarnContext(ctx, "profile overview: contribution org repos", "org_id", org.OrgID, "error", err)
				continue
			}
			for _, repo := range orgRepos {
				add(org.Slug, repo, true, true)
			}
		}
	}

	publicRepos, err := queries.ListPublicContributionRepos(ctx, h.d.Pool, int32(profileContribRepoLimit))
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile overview: contribution public repos", "user_id", user.ID, "error", err)
		return out
	}
	for _, row := range publicRepos {
		add(row.OwnerSlug, row.Repo, false, false)
	}
	return out
}

func selectedContributionYear(query url.Values, currentYear int) int {
	for _, key := range []string{"year", "from"} {
		raw := strings.TrimSpace(query.Get(key))
		if len(raw) >= 4 {
			raw = raw[:4]
		}
		year, err := strconv.Atoi(raw)
		if err == nil && year >= currentYear-100 && year <= currentYear {
			return year
		}
	}
	return currentYear
}

func contributionGrid(windowStart, windowEndDay time.Time) (time.Time, int) {
	gridStart := windowStart.AddDate(0, 0, -int(windowStart.Weekday()))
	gridEnd := windowEndDay.AddDate(0, 0, 6-int(windowEndDay.Weekday()))
	days := int(gridEnd.Sub(gridStart).Hours()/24) + 1
	if days <= 0 {
		return gridStart, 0
	}
	return gridStart, days / 7
}

func contributionWeekMonthLabel(weekStart time.Time, firstWeek bool) string {
	for offset := 0; offset < 7; offset++ {
		day := weekStart.AddDate(0, 0, offset)
		if day.Day() == 1 {
			return day.Format("Jan")
		}
	}
	if firstWeek {
		return weekStart.Format("Jan")
	}
	return ""
}

func contributionYears(username string, currentYear, selectedYear int) []contributionYear {
	years := make([]contributionYear, 0, 4)
	base := "/" + url.PathEscape(username)
	for i := 0; i < 4; i++ {
		year := currentYear - i
		href := base
		if year != currentYear {
			href += "?year=" + strconv.Itoa(year)
		}
		years = append(years, contributionYear{
			Year:   year,
			Href:   href,
			Active: year == selectedYear,
		})
	}
	return years
}

func commitMatchesProfileUser(commit repogit.Commit, verifiedEmails map[string]struct{}, user usersdb.User, allowIdentityFallback bool) bool {
	if len(verifiedEmails) > 0 {
		if _, ok := verifiedEmails[strings.ToLower(strings.TrimSpace(commit.AuthorEmail))]; ok {
			return true
		}
	}
	if !allowIdentityFallback {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(commit.AuthorName))
	if name == strings.ToLower(user.Username) {
		return true
	}
	if user.DisplayName != "" && name == strings.ToLower(strings.TrimSpace(user.DisplayName)) {
		return true
	}
	return commitEmailNamesProfileUser(commit.AuthorEmail, user.Username)
}

func commitEmailNamesProfileUser(email, username string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	username = strings.ToLower(strings.TrimSpace(username))
	if email == "" || username == "" {
		return false
	}
	return strings.HasSuffix(email, "+"+username+"@users.noreply.github.com") ||
		email == username+"@users.noreply.github.com"
}

func (h *Handlers) verifiedEmails(ctx context.Context, userID int64) map[string]struct{} {
	rows, err := h.q.ListUserEmailsForUser(ctx, h.d.Pool, userID)
	if err != nil {
		return nil
	}
	out := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Verified {
			out[strings.ToLower(strings.TrimSpace(row.Email))] = struct{}{}
		}
	}
	return out
}

func contributionLevel(count int) int {
	switch {
	case count <= 0:
		return 0
	case count < 3:
		return 1
	case count < 6:
		return 2
	case count < 10:
		return 3
	default:
		return 4
	}
}

func contributionDayTitle(count int, day time.Time) string {
	date := day.Format("January ") + ordinalDay(day.Day()) + "."
	if count == 0 {
		return "No contributions on " + date
	}
	if count == 1 {
		return "1 contribution on " + date
	}
	return strconv.Itoa(count) + " contributions on " + date
}

func ordinalDay(day int) string {
	suffix := "th"
	if day%100 < 11 || day%100 > 13 {
		switch day % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(day) + suffix
}

func profileCodeRouteBase(owner, repoName, route, ref, dir string) string {
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/" + route + "/" + escapeProfilePathSegments(ref)
	if dir != "" {
		base += "/" + escapeProfilePathSegments(dir)
	}
	return base
}

func escapeProfilePathSegments(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func rewriteProfileMarkdownRelativeURLs(fragment, linkBase, linkRoot, imageBase, imageRoot string) string {
	if fragment == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(fragment))
	var out bytes.Buffer
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() == io.EOF {
				return out.String()
			}
			return fragment
		}
		tok := z.Token()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			rewriteProfileMarkdownTokenURLs(&tok, linkBase, linkRoot, imageBase, imageRoot)
		}
		out.WriteString(tok.String())
	}
}

func rewriteProfileMarkdownTokenURLs(tok *html.Token, linkBase, linkRoot, imageBase, imageRoot string) {
	switch tok.Data {
	case "a":
		rewriteProfileAttr(tok, "href", linkBase, linkRoot)
	case "img":
		rewriteProfileAttr(tok, "src", imageBase, imageRoot)
	}
}

func rewriteProfileAttr(tok *html.Token, key, base, root string) {
	for i := range tok.Attr {
		if tok.Attr[i].Key == key {
			tok.Attr[i].Val = rewriteProfileRelativeMarkdownURL(tok.Attr[i].Val, base, root)
		}
	}
}

func rewriteProfileRelativeMarkdownURL(raw, base, root string) string {
	if raw == "" || base == "" || root == "" || strings.TrimSpace(raw) != raw {
		return raw
	}
	if strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "/") || u.Path == "" {
		return raw
	}
	next := path.Clean(path.Clean(base) + "/" + u.Path)
	if next != root && !strings.HasPrefix(next, root+"/") {
		return raw
	}
	u.Path = next
	u.RawPath = ""
	return u.String()
}
