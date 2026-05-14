// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	orgHomepageRepoLimit   = 10
	orgHomepagePinnedLimit = 6
	orgHomepagePeopleLimit = 8
)

type orgProfileRepo struct {
	ID                   int64
	Name                 string
	Description          string
	Visibility           string
	IsArchived           bool
	IsFork               bool
	Public               bool
	Private              bool
	Source               bool
	Archived             bool
	PrimaryLanguage      string
	PrimaryLanguageColor template.CSS
	LicenseKey           string
	StarCount            int64
	ForkCount            int64
	UpdatedAt            time.Time
	Topics               []string
	DefaultBranch        string
	ActivitySparkline    template.HTML
}

type orgProfilePerson struct {
	Username    string
	DisplayName string
	Role        string
	AvatarURL   string
}

type orgProfileLanguage struct {
	Name    string
	Color   template.CSS
	Count   int
	Percent int
}

type orgProfileTopic struct {
	Name  string
	Count int
}

// serveOrgProfile renders /{org}. It mirrors GitHub's organization
// overview shape: org nav, pinned repo cards, recent repo rows, and a
// right rail with people/language/topic aggregates.
func (h *Handlers) serveOrgProfile(w http.ResponseWriter, r *http.Request, orgID int64) {
	ctx := r.Context()
	q := orgsdb.New()
	org, err := q.GetOrgByID(ctx, h.d.Pool, orgID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	if org.DeletedAt.Valid {
		// Soft-deleted orgs render the same "unavailable" shell as
		// suspended/deleted users so the existence-leak posture is
		// uniform.
		h.renderUnavailable(w, r, string(org.Slug))
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	isOwner := false
	isMember := false
	if !viewer.IsAnonymous() {
		deps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
		isOwner, _ = orgs.IsOwner(ctx, deps, org.ID, viewer.ID)
		isMember, _ = orgs.IsMember(ctx, deps, org.ID, viewer.ID)
	}

	repos := h.orgProfileRepos(ctx, org.ID, viewer)
	followState := h.orgFollowState(ctx, org.ID, viewer)
	repoRows := h.withOrgRepoActivity(ctx, string(org.Slug), limitOrgRepos(repos, orgHomepageRepoLimit))
	pinnedRepos, pinCandidates := h.orgPinData(ctx, org.ID, string(org.Slug), repos)
	people := h.orgProfilePeople(ctx, q, org.ID)
	memberCount := int64(len(people))
	teamCount := int64(0)
	if isMember || viewer.IsSiteAdmin {
		_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM teams WHERE org_id = $1`, org.ID).Scan(&teamCount)
	}
	viewAs := "Public"
	switch {
	case !viewer.IsAnonymous() && viewer.IsSiteAdmin:
		viewAs = "Site admin"
	case isOwner:
		viewAs = "Owner"
	case isMember:
		viewAs = "Member"
	}

	avatarURL := "/avatars/" + url.PathEscape(org.Slug)
	data := map[string]any{
		"Title":              org.DisplayName,
		"OGTitle":            org.DisplayName,
		"OGDescription":      org.Description,
		"OGImage":            avatarURL,
		"Org":                org,
		"AvatarURL":          avatarURL,
		"ActiveOrgNav":       "overview",
		"WebsiteSafe":        safeWebsite(org.Website),
		"Repos":              repoRows,
		"HasMoreRepos":       len(repos) > orgHomepageRepoLimit,
		"OrgRepositoriesURL": orgRepositoriesBaseURL(string(org.Slug)),
		"PinnedRepos":        pinnedRepos,
		"PinCandidates":      pinCandidates,
		"PinsRemaining":      profilePinsRemaining(pinCandidates, profilePinLimit),
		"MaxPins":            int64(profilePinLimit),
		"RepoCount":          int64(len(repos)),
		"TeamCount":          teamCount,
		"MemberCount":        memberCount,
		"FollowerCount":      followState.FollowersCount,
		"IsFollowing":        followState.IsFollowing,
		"FollowAction":       "/" + url.PathEscape(org.Slug) + "/follow",
		"UnfollowAction":     "/" + url.PathEscape(org.Slug) + "/unfollow",
		"ReturnTo":           r.URL.RequestURI(),
		"People":             limitOrgPeople(people, orgHomepagePeopleLimit),
		"TopLanguages":       orgTopLanguages(repos),
		"TopTopics":          orgTopTopics(repos),
		"ViewAs":             viewAs,
		"IsOwner":            isOwner,
		"IsMember":           isMember,
		"CanCustomizePins":   isOwner,
		"PinsAction":         "/" + url.PathEscape(org.Slug) + "/pins",
		"CanCreateRepo":      isOwner || (isMember && org.AllowMemberRepoCreate),
	}
	if !viewer.IsAnonymous() {
		w.Header().Set("Cache-Control", "no-cache, private")
	} else {
		w.Header().Set("Cache-Control", "max-age=120")
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/profile", data); err != nil {
		h.d.Logger.ErrorContext(ctx, "orgs profile: render", "error", err)
	}
}

func (h *Handlers) orgProfileRepos(ctx context.Context, orgID int64, viewer middleware.CurrentUser) []orgProfileRepo {
	rows, err := reposdb.New().ListReposForOwnerOrg(ctx, h.d.Pool, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "orgs profile: list repos", "error", err)
		return nil
	}
	actor := policy.AnonymousActor()
	if !viewer.IsAnonymous() {
		actor = viewer.PolicyActor()
	}
	deps := policy.Deps{Pool: h.d.Pool}

	out := make([]orgProfileRepo, 0, len(rows))
	for _, row := range rows {
		repoRef := policy.NewRepoRefFromRepo(row)
		if !policy.IsVisibleTo(ctx, deps, actor, repoRef) {
			continue
		}
		item := orgProfileRepo{
			ID:              row.ID,
			Name:            string(row.Name),
			Description:     row.Description,
			Visibility:      repoRef.Visibility,
			IsArchived:      repoRef.IsArchived,
			IsFork:          row.ForkOfRepoID.Valid,
			Public:          repoRef.IsPublic(),
			Private:         repoRef.IsPrivate(),
			Source:          !row.ForkOfRepoID.Valid && !repoRef.IsArchived,
			Archived:        repoRef.IsArchived,
			LicenseKey:      pgTextStringOrEmpty(row.LicenseKey),
			StarCount:       row.StarCount,
			ForkCount:       row.ForkCount,
			UpdatedAt:       row.UpdatedAt.Time,
			Topics:          h.orgRepoTopics(ctx, row.ID),
			PrimaryLanguage: pgTextStringOrEmpty(row.PrimaryLanguage),
			DefaultBranch:   row.DefaultBranch,
		}
		item.PrimaryLanguageColor = template.CSS(orgLanguageColor(item.PrimaryLanguage)) //nolint:gosec // CSS value comes from server-side constants.
		out = append(out, item)
	}
	return out
}

func (h *Handlers) withOrgRepoActivity(ctx context.Context, orgSlug string, repos []orgProfileRepo) []orgProfileRepo {
	out := append([]orgProfileRepo(nil), repos...)
	for i := range out {
		out[i].ActivitySparkline = h.orgRepoActivitySparkline(ctx, orgSlug, out[i])
	}
	return out
}

func (h *Handlers) orgRepoActivitySparkline(ctx context.Context, orgSlug string, repo orgProfileRepo) template.HTML {
	if h.d.RepoFS == nil {
		return orgActivitySparklineSVG(nil)
	}
	gitDir, err := h.d.RepoFS.RepoPath(orgSlug, repo.Name)
	if err != nil {
		return orgActivitySparklineSVG(nil)
	}
	buckets, err := repogit.WeeklyCommitActivity(ctx, gitDir, repo.DefaultBranch, 52, time.Now())
	if err != nil {
		return orgActivitySparklineSVG(nil)
	}
	return orgActivitySparklineSVG(buckets)
}

func orgActivitySparklineSVG(buckets []int) template.HTML {
	const (
		width    = 155.0
		baseline = 27.0
		top      = 5.0
	)
	if len(buckets) < 2 {
		buckets = make([]int, 52)
	}
	maxCount := 0
	for _, count := range buckets {
		if count > maxCount {
			maxCount = count
		}
	}

	step := width / float64(len(buckets)-1)
	points := make([]string, 0, len(buckets))
	for i, count := range buckets {
		y := baseline
		if maxCount > 0 && count > 0 {
			ratio := math.Sqrt(float64(count)) / math.Sqrt(float64(maxCount))
			y = baseline - ratio*(baseline-top)
		}
		points = append(points, fmt.Sprintf("%.1f %.1f", float64(i)*step, y))
	}

	var b strings.Builder
	b.WriteString(`<svg class="shithub-org-repo-spark" viewBox="0 0 155 32" width="155" height="32" aria-hidden="true" focusable="false">`)
	b.WriteString(`<path class="shithub-org-repo-spark-base" d="M 0 27 H 155"></path>`)
	b.WriteString(`<polyline class="shithub-org-repo-spark-line" points="`)
	b.WriteString(strings.Join(points, " "))
	b.WriteString(`"></polyline>`)
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // SVG contains only server-generated numeric points.
}

func (h *Handlers) orgRepoTopics(ctx context.Context, repoID int64) []string {
	topics, err := reposdb.New().ListRepoTopics(ctx, h.d.Pool, repoID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "orgs profile: list repo topics", "repo_id", repoID, "error", err)
		return nil
	}
	return topics
}

func (h *Handlers) orgProfilePeople(ctx context.Context, q *orgsdb.Queries, orgID int64) []orgProfilePerson {
	rows, err := q.ListOrgMembers(ctx, h.d.Pool, orgID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "orgs profile: list people", "org_id", orgID, "error", err)
		return nil
	}
	out := make([]orgProfilePerson, 0, len(rows))
	for _, row := range rows {
		out = append(out, orgProfilePerson{
			Username:    row.Username,
			DisplayName: row.DisplayName,
			Role:        string(row.Role),
			AvatarURL:   "/avatars/" + url.PathEscape(row.Username),
		})
	}
	return out
}

func limitOrgRepos(repos []orgProfileRepo, limit int) []orgProfileRepo {
	if len(repos) <= limit {
		return repos
	}
	return repos[:limit]
}

func pinnedOrgRepos(repos []orgProfileRepo) []orgProfileRepo {
	pinned := append([]orgProfileRepo(nil), repos...)
	sort.SliceStable(pinned, func(i, j int) bool {
		if pinned[i].StarCount != pinned[j].StarCount {
			return pinned[i].StarCount > pinned[j].StarCount
		}
		return pinned[i].UpdatedAt.After(pinned[j].UpdatedAt)
	})
	return limitOrgRepos(pinned, orgHomepagePinnedLimit)
}

func limitOrgPeople(people []orgProfilePerson, limit int) []orgProfilePerson {
	if len(people) <= limit {
		return people
	}
	return people[:limit]
}

func orgTopLanguages(repos []orgProfileRepo) []orgProfileLanguage {
	counts := map[string]int{}
	for _, repo := range repos {
		if repo.PrimaryLanguage == "" {
			continue
		}
		counts[repo.PrimaryLanguage]++
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	out := make([]orgProfileLanguage, 0, len(counts))
	for name, n := range counts {
		percent := 0
		if total > 0 {
			percent = int(float64(n) / float64(total) * 100)
			if percent == 0 {
				percent = 1
			}
		}
		out = append(out, orgProfileLanguage{
			Name:    name,
			Color:   template.CSS(orgLanguageColor(name)), //nolint:gosec // CSS value comes from server-side constants.
			Count:   n,
			Percent: percent,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 5 {
		return out[:5]
	}
	return out
}

func orgTopTopics(repos []orgProfileRepo) []orgProfileTopic {
	counts := map[string]int{}
	for _, repo := range repos {
		for _, topic := range repo.Topics {
			counts[topic]++
		}
	}
	out := make([]orgProfileTopic, 0, len(counts))
	for name, n := range counts {
		out = append(out, orgProfileTopic{Name: name, Count: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 8 {
		return out[:8]
	}
	return out
}

func orgLanguageColor(name string) string {
	switch name {
	case "Go":
		return "#00add8"
	case "HTML":
		return "#e34c26"
	case "CSS":
		return "#663399"
	case "Shell":
		return "#89e051"
	case "PLpgSQL":
		return "#336790"
	case "Jinja":
		return "#a52a22"
	case "JavaScript":
		return "#f1e05a"
	case "TypeScript":
		return "#3178c6"
	case "Python":
		return "#3572a5"
	case "Java":
		return "#b07219"
	case "Rust":
		return "#dea584"
	case "Ruby":
		return "#701516"
	case "PHP":
		return "#4f5d95"
	case "C":
		return "#555555"
	case "C++":
		return "#f34b7d"
	case "Makefile":
		return "#427819"
	case "Dockerfile":
		return "#384d54"
	default:
		return "#ededed"
	}
}
