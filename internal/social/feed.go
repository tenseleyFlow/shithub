// SPDX-License-Identifier: AGPL-3.0-or-later

package social

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

const (
	defaultFeedLimit     int32 = 30
	defaultTrendingLimit int32 = 10
)

// FeedCursor is S42's keyset cursor over (created_at, event_id).
type FeedCursor struct {
	BeforeCreatedAt time.Time
	BeforeID        int64
}

type FeedRepo struct {
	ID              int64
	Owner           string
	Name            string
	Description     string
	PrimaryLanguage string
	StarCount       int64
	ForkCount       int64
}

type FeedItem struct {
	ID               int64
	Kind             string
	Verb             string
	ActorUsername    string
	ActorDisplayName string
	CreatedAt        time.Time
	Repo             *FeedRepo
	RepoFullName     string
	RepoURL          string
	SourceName       string
	SourceURL        string
	ItemTitle        string
	ItemURL          string
}

type DashboardRepo struct {
	ID              int64
	Name            string
	Description     string
	Visibility      string
	PrimaryLanguage string
	StarCount       int64
	ForkCount       int64
	UpdatedAt       time.Time
}

type TrendingRepo struct {
	RepoID          int64  `json:"repo_id"`
	Owner           string `json:"owner"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	PrimaryLanguage string `json:"primary_language,omitempty"`
	StarCount       int64  `json:"star_count"`
	ForkCount       int64  `json:"fork_count"`
	Score           int64  `json:"score"`
}

type TrendingUser struct {
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	Score         int64  `json:"score"`
	FollowerDelta int64  `json:"follower_delta"`
	EventCount    int64  `json:"event_count"`
}

// DashboardFeed returns public feed rows from followed users, followed
// orgs, watched repos, and the viewer's own public activity.
func DashboardFeed(ctx context.Context, deps Deps, viewerUserID int64, cursor FeedCursor, limit int32) ([]FeedItem, error) {
	if viewerUserID == 0 {
		return nil, ErrNotLoggedIn
	}
	if limit <= 0 || limit > 100 {
		limit = defaultFeedLimit
	}
	params := socialdb.ListDashboardFeedEventsParams{
		ViewerUserID: viewerUserID,
		LimitCount:   limit,
	}
	if !cursor.BeforeCreatedAt.IsZero() && cursor.BeforeID > 0 {
		params.BeforeCreatedAt = pgtype.Timestamptz{Time: cursor.BeforeCreatedAt, Valid: true}
		params.BeforeID = pgtype.Int8{Int64: cursor.BeforeID, Valid: true}
	}
	rows, err := socialdb.New().ListDashboardFeedEvents(ctx, deps.Pool, params)
	if err != nil {
		return nil, fmt.Errorf("dashboard feed: %w", err)
	}
	out := make([]FeedItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, feedItemFromDashboardRow(row))
	}
	return out, nil
}

// PublicFeed returns the global public activity feed used by Explore.
func PublicFeed(ctx context.Context, deps Deps, cursor FeedCursor, limit int32) ([]FeedItem, error) {
	if limit <= 0 || limit > 100 {
		limit = defaultFeedLimit
	}
	params := socialdb.ListPublicFeedEventsParams{LimitCount: limit}
	if !cursor.BeforeCreatedAt.IsZero() && cursor.BeforeID > 0 {
		params.BeforeCreatedAt = pgtype.Timestamptz{Time: cursor.BeforeCreatedAt, Valid: true}
		params.BeforeID = pgtype.Int8{Int64: cursor.BeforeID, Valid: true}
	}
	rows, err := socialdb.New().ListPublicFeedEvents(ctx, deps.Pool, params)
	if err != nil {
		return nil, fmt.Errorf("public feed: %w", err)
	}
	out := make([]FeedItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, feedItemFromPublicRow(row))
	}
	return out, nil
}

func DashboardRepos(ctx context.Context, deps Deps, viewerUserID int64, limit int32) ([]DashboardRepo, error) {
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	rows, err := socialdb.New().ListDashboardReposForUser(ctx, deps.Pool, socialdb.ListDashboardReposForUserParams{
		OwnerUserID: pgtype.Int8{Int64: viewerUserID, Valid: true},
		Limit:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("dashboard repos: %w", err)
	}
	out := make([]DashboardRepo, 0, len(rows))
	for _, row := range rows {
		out = append(out, DashboardRepo{
			ID: row.RepoID, Name: row.Name, Description: row.Description,
			Visibility: string(row.Visibility), PrimaryLanguage: row.PrimaryLanguage,
			StarCount: row.StarCount, ForkCount: row.ForkCount,
			UpdatedAt: timeFromPG(row.UpdatedAt),
		})
	}
	return out, nil
}

func TrendingRepos(ctx context.Context, deps Deps, windowDays, limit int32) ([]TrendingRepo, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	if limit <= 0 || limit > 50 {
		limit = defaultTrendingLimit
	}
	rows, err := socialdb.New().ListTrendingRepos(ctx, deps.Pool, socialdb.ListTrendingReposParams{
		LimitCount: limit,
		WindowDays: windowDays,
	})
	if err != nil {
		return nil, fmt.Errorf("trending repos: %w", err)
	}
	out := make([]TrendingRepo, 0, len(rows))
	for _, row := range rows {
		out = append(out, TrendingRepo{
			RepoID: row.RepoID, Owner: row.Owner, Name: row.Name,
			Description: row.Description, PrimaryLanguage: row.PrimaryLanguage,
			StarCount: row.StarCount, ForkCount: row.ForkCount, Score: row.Score,
		})
	}
	return out, nil
}

func TrendingUsers(ctx context.Context, deps Deps, windowDays, limit int32) ([]TrendingUser, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	if limit <= 0 || limit > 50 {
		limit = defaultTrendingLimit
	}
	rows, err := socialdb.New().ListTrendingUsers(ctx, deps.Pool, socialdb.ListTrendingUsersParams{
		LimitCount: limit,
		WindowDays: windowDays,
	})
	if err != nil {
		return nil, fmt.Errorf("trending users: %w", err)
	}
	out := make([]TrendingUser, 0, len(rows))
	for _, row := range rows {
		out = append(out, TrendingUser{
			UserID: row.UserID, Username: row.Username, DisplayName: row.DisplayName,
			Score: row.Score, FollowerDelta: row.FollowerDelta, EventCount: row.EventCount,
		})
	}
	return out, nil
}

// CaptureTrendingSnapshots computes the S42 denormalized rankings for
// day/week/month windows. It is idempotent in behavior: inserting a new
// snapshot never mutates prior rows, so stale readers still have a valid
// last-known ranking.
func CaptureTrendingSnapshots(ctx context.Context, deps Deps) error {
	q := socialdb.New()
	windows := []struct {
		scope socialdb.TrendingScope
		days  int32
	}{
		{scope: socialdb.TrendingScopeDay, days: 1},
		{scope: socialdb.TrendingScopeWeek, days: 7},
		{scope: socialdb.TrendingScopeMonth, days: 30},
	}
	for _, window := range windows {
		repos, err := TrendingRepos(ctx, deps, window.days, 50)
		if err != nil {
			return err
		}
		body, err := json.Marshal(repos)
		if err != nil {
			return fmt.Errorf("marshal trending repos: %w", err)
		}
		if _, err := q.InsertTrendingSnapshot(ctx, deps.Pool, socialdb.InsertTrendingSnapshotParams{
			Scope: window.scope, Kind: socialdb.TrendingKindRepos, Payload: body,
		}); err != nil {
			return fmt.Errorf("insert trending repos snapshot: %w", err)
		}

		users, err := TrendingUsers(ctx, deps, window.days, 50)
		if err != nil {
			return err
		}
		body, err = json.Marshal(users)
		if err != nil {
			return fmt.Errorf("marshal trending users: %w", err)
		}
		if _, err := q.InsertTrendingSnapshot(ctx, deps.Pool, socialdb.InsertTrendingSnapshotParams{
			Scope: window.scope, Kind: socialdb.TrendingKindUsers, Payload: body,
		}); err != nil {
			return fmt.Errorf("insert trending users snapshot: %w", err)
		}
	}
	return nil
}

func feedItemFromDashboardRow(row socialdb.ListDashboardFeedEventsRow) FeedItem {
	return feedItemFromParts(feedParts{
		id: row.ID, kind: row.Kind, actorUsername: row.ActorUsername,
		actorDisplayName: row.ActorDisplayName, createdAt: row.CreatedAt,
		repoID: row.RepoID, repoOwner: row.RepoOwner, repoName: row.RepoName,
		repoDescription: row.RepoDescription, repoPrimaryLanguage: row.RepoPrimaryLanguage,
		repoStarCount: row.RepoStarCount, repoForkCount: row.RepoForkCount,
		sourceName: row.SourceName, payload: row.Payload,
	})
}

func feedItemFromPublicRow(row socialdb.ListPublicFeedEventsRow) FeedItem {
	return feedItemFromParts(feedParts{
		id: row.ID, kind: row.Kind, actorUsername: row.ActorUsername,
		actorDisplayName: row.ActorDisplayName, createdAt: row.CreatedAt,
		repoID: row.RepoID, repoOwner: row.RepoOwner, repoName: row.RepoName,
		repoDescription: row.RepoDescription, repoPrimaryLanguage: row.RepoPrimaryLanguage,
		repoStarCount: row.RepoStarCount, repoForkCount: row.RepoForkCount,
		sourceName: row.SourceName, payload: row.Payload,
	})
}

type feedParts struct {
	id                  int64
	kind                string
	actorUsername       string
	actorDisplayName    string
	createdAt           pgtype.Timestamptz
	repoID              pgtype.Int8
	repoOwner           string
	repoName            string
	repoDescription     string
	repoPrimaryLanguage string
	repoStarCount       int64
	repoForkCount       int64
	sourceName          string
	payload             []byte
}

func feedItemFromParts(p feedParts) FeedItem {
	item := FeedItem{
		ID: p.id, Kind: p.kind, Verb: feedVerb(p.kind),
		ActorUsername: p.actorUsername, ActorDisplayName: p.actorDisplayName,
		CreatedAt: timeFromPG(p.createdAt), SourceName: p.sourceName,
	}
	if p.repoID.Valid && p.repoOwner != "" && p.repoName != "" {
		item.Repo = &FeedRepo{
			ID: p.repoID.Int64, Owner: p.repoOwner, Name: p.repoName,
			Description: p.repoDescription, PrimaryLanguage: p.repoPrimaryLanguage,
			StarCount: p.repoStarCount, ForkCount: p.repoForkCount,
		}
		item.RepoFullName = p.repoOwner + "/" + p.repoName
		item.RepoURL = "/" + item.RepoFullName
	}
	item.ItemTitle, item.ItemURL = feedItemTarget(p.kind, p.payload, item)
	if item.SourceName != "" {
		item.SourceURL = "/" + item.SourceName
	}
	return item
}

func feedVerb(kind string) string {
	switch kind {
	case "star":
		return "starred"
	case "unstar":
		return "unstarred"
	case "forked":
		return "forked"
	case "push":
		return "pushed to"
	case "repo_created":
		return "created"
	case "issue_created":
		return "opened an issue in"
	case "issue_comment_created":
		return "commented on an issue in"
	case "issue_closed":
		return "closed an issue in"
	case "issue_reopened":
		return "reopened an issue in"
	case "pr_opened":
		return "opened a pull request in"
	case "pr_comment_created":
		return "commented on a pull request in"
	case "pr_closed":
		return "closed a pull request in"
	case "pr_reopened":
		return "reopened a pull request in"
	case "pr_merged":
		return "merged a pull request in"
	case "followed_user", "followed_org":
		return "followed"
	default:
		return strings.ReplaceAll(kind, "_", " ")
	}
}

func feedItemTarget(kind string, payload []byte, item FeedItem) (string, string) {
	switch kind {
	case "followed_user", "followed_org":
		if item.SourceName != "" {
			return item.SourceName, "/" + item.SourceName
		}
		return "a profile", ""
	}
	data := map[string]any{}
	_ = json.Unmarshal(payload, &data)
	if title, _ := data["issue_title"].(string); title != "" {
		if item.RepoURL == "" {
			return title, ""
		}
		if n, ok := jsonNumberToInt(data["issue_number"]); ok {
			section := "issues"
			if strings.HasPrefix(kind, "pr_") {
				section = "pulls"
			}
			return title, item.RepoURL + "/" + section + "/" + strconv.FormatInt(n, 10)
		}
		return title, item.RepoURL
	}
	if item.RepoFullName != "" {
		return item.RepoFullName, item.RepoURL
	}
	return "", ""
}

func jsonNumberToInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func timeFromPG(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}
