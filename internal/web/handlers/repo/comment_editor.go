// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"html/template"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type commentEditorConfig struct {
	Mentions   []commentEditorMention   `json:"mentions"`
	References []commentEditorReference `json:"references"`
}

type commentEditorMention struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
}

type commentEditorReference struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
}

func commentEditorConfigJSON(config commentEditorConfig) template.JS {
	body, err := json.Marshal(config)
	if err != nil {
		return template.JS(`{"mentions":[],"references":[]}`) //nolint:gosec // constant fallback
	}
	return template.JS(body) //nolint:gosec // json.Marshal escapes script-breaking characters
}

func commentEditorAvatarURL(username string) string {
	if strings.TrimSpace(username) == "" {
		return ""
	}
	return "/avatars/" + url.PathEscape(username)
}

func (h *Handlers) pullCommentEditorConfig(
	ctx context.Context,
	row reposdb.Repo,
	pr pullsdb.GetPullRequestByRepoAndNumberRow,
	viewer middleware.CurrentUser,
	comments []issuesdb.IssueComment,
	assignees []issuesdb.ListIssueAssigneesRow,
	reviews []pullsdb.PrReview,
	requests []pullsdb.PrReviewRequest,
) commentEditorConfig {
	config := commentEditorConfig{}
	mentions := map[string]commentEditorMention{}

	addUserID := func(id int64) {
		if id == 0 {
			return
		}
		u, err := h.uq.GetUserByID(ctx, h.d.Pool, id)
		if err != nil || strings.EqualFold(u.Username, "copilot") {
			return
		}
		mentions[strings.ToLower(u.Username)] = commentEditorMention{
			Username:    u.Username,
			DisplayName: u.DisplayName,
			AvatarURL:   commentEditorAvatarURL(u.Username),
		}
	}
	addUsername := func(username, displayName string) {
		username = strings.TrimSpace(username)
		if username == "" || strings.EqualFold(username, "copilot") {
			return
		}
		key := strings.ToLower(username)
		if _, ok := mentions[key]; ok {
			return
		}
		mentions[key] = commentEditorMention{
			Username:    username,
			DisplayName: displayName,
			AvatarURL:   commentEditorAvatarURL(username),
		}
	}

	addUsername(viewer.Username, "")
	if pr.IAuthorUserID.Valid {
		addUserID(pr.IAuthorUserID.Int64)
	}
	if pr.MergedByUserID.Valid {
		addUserID(pr.MergedByUserID.Int64)
	}
	for _, comment := range comments {
		if comment.AuthorUserID.Valid {
			addUserID(comment.AuthorUserID.Int64)
		}
	}
	for _, assignee := range assignees {
		addUsername(assignee.Username, assignee.DisplayName)
	}
	for _, review := range reviews {
		if review.AuthorUserID.Valid {
			addUserID(review.AuthorUserID.Int64)
		}
	}
	for _, request := range requests {
		if request.RequestedUserID.Valid {
			addUserID(request.RequestedUserID.Int64)
		}
		if request.RequestedByUserID.Valid {
			addUserID(request.RequestedByUserID.Int64)
		}
	}

	config.Mentions = make([]commentEditorMention, 0, len(mentions))
	for _, mention := range mentions {
		config.Mentions = append(config.Mentions, mention)
	}
	sort.SliceStable(config.Mentions, func(i, j int) bool {
		if strings.EqualFold(config.Mentions[i].Username, viewer.Username) {
			return true
		}
		if strings.EqualFold(config.Mentions[j].Username, viewer.Username) {
			return false
		}
		return strings.ToLower(config.Mentions[i].Username) < strings.ToLower(config.Mentions[j].Username)
	})

	seenRefs := map[int64]struct{}{}
	addRef := func(number int64, title, kind, state string) {
		if number == 0 {
			return
		}
		if _, ok := seenRefs[number]; ok {
			return
		}
		seenRefs[number] = struct{}{}
		config.References = append(config.References, commentEditorReference{
			Number: number,
			Title:  title,
			Kind:   kind,
			State:  state,
		})
	}
	addRef(pr.INumber, pr.ITitle, "pull request", string(pr.IState))
	if prs, err := h.pq.ListPullRequestsByRepo(ctx, h.d.Pool, pullsdb.ListPullRequestsByRepoParams{
		RepoID:      row.ID,
		Limit:       8,
		StateFilter: pgtype.Text{},
		Draft:       pgtype.Bool{},
	}); err == nil {
		for _, item := range prs {
			addRef(item.Number, item.Title, "pull request", string(item.State))
		}
	}
	if issues, err := h.iq.ListIssues(ctx, h.d.Pool, issuesdb.ListIssuesParams{
		RepoID:      row.ID,
		Limit:       8,
		StateFilter: pgtype.Text{},
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	}); err == nil {
		for _, item := range issues {
			addRef(item.Number, item.Title, "issue", string(item.State))
		}
	}
	if len(config.References) > 10 {
		config.References = config.References[:10]
	}

	return config
}
