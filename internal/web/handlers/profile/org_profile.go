// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
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
	PrimaryLanguage      string
	PrimaryLanguageColor template.CSS
	LicenseKey           string
	StarCount            int64
	ForkCount            int64
	UpdatedAt            time.Time
	Topics               []string
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
	people := h.orgProfilePeople(ctx, q, org.ID)
	memberCount := h.orgMemberCount(ctx, org.ID)
	viewAs := "Public"
	if isOwner {
		viewAs = "Owner"
	} else if isMember {
		viewAs = "Member"
	}

	avatarURL := "/avatars/" + url.PathEscape(org.Slug)
	data := map[string]any{
		"Title":         org.DisplayName,
		"OGTitle":       org.DisplayName,
		"OGDescription": org.Description,
		"OGImage":       avatarURL,
		"Org":           org,
		"AvatarURL":     avatarURL,
		"WebsiteSafe":   safeWebsite(org.Website),
		"Repos":         limitOrgRepos(repos, orgHomepageRepoLimit),
		"PinnedRepos":   pinnedOrgRepos(repos),
		"RepoCount":     int64(len(repos)),
		"MemberCount":   memberCount,
		"People":        limitOrgPeople(people, orgHomepagePeopleLimit),
		"TopLanguages":  orgTopLanguages(repos),
		"TopTopics":     orgTopTopics(repos),
		"ViewAs":        viewAs,
		"IsOwner":       isOwner,
		"IsMember":      isMember,
		"CanCreateRepo": isOwner || (isMember && org.AllowMemberRepoCreate),
	}
	if isMember {
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
		actor = policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, viewer.IsSiteAdmin)
		if viewer.ImpersonatedUserID != 0 {
			actor.Impersonating = true
			actor.ImpersonateWriteOK = viewer.ImpersonateWriteOK
		}
	}
	deps := policy.Deps{Pool: h.d.Pool}

	out := make([]orgProfileRepo, 0, len(rows))
	for _, row := range rows {
		if !policy.IsVisibleTo(ctx, deps, actor, policy.NewRepoRefFromRepo(row)) {
			continue
		}
		item := orgProfileRepo{
			ID:              row.ID,
			Name:            string(row.Name),
			Description:     row.Description,
			Visibility:      string(row.Visibility),
			IsArchived:      row.IsArchived,
			IsFork:          row.ForkOfRepoID.Valid,
			LicenseKey:      pgTextStringOrEmpty(row.LicenseKey),
			StarCount:       row.StarCount,
			ForkCount:       row.ForkCount,
			UpdatedAt:       row.UpdatedAt.Time,
			Topics:          h.orgRepoTopics(ctx, row.ID),
			PrimaryLanguage: pgTextStringOrEmpty(row.PrimaryLanguage),
		}
		item.PrimaryLanguageColor = template.CSS(orgLanguageColor(item.PrimaryLanguage)) //nolint:gosec // CSS value comes from server-side constants.
		out = append(out, item)
	}
	return out
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

func (h *Handlers) orgMemberCount(ctx context.Context, orgID int64) int64 {
	var n int64
	_ = h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, orgID).Scan(&n)
	return n
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
