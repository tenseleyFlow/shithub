// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

const feedDisplayLimit int32 = 20

type exploreHandler struct {
	render *render.Renderer
	logger *slog.Logger
	pool   *pgxpool.Pool
}

func (h exploreHandler) ServeExplore(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "Explore", "/explore", "activity")
}

func (h exploreHandler) ServeTrending(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "Trending", "/trending", "trending")
}

func (h exploreHandler) serve(w http.ResponseWriter, r *http.Request, title, path, activeTab string) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	var (
		feed          []social.FeedItem
		hasNext       bool
		nextURL       string
		topRepos      []social.DashboardRepo
		viewerOrgs    []orgsdb.ListOrgsForUserRow
		trendingRepos []social.TrendingRepo
		trendingUsers []social.TrendingUser
	)
	if h.pool != nil {
		deps := social.Deps{Pool: h.pool, Logger: h.logger}
		feed, hasNext, nextURL = feedPageFor(r, func(cursor social.FeedCursor, limit int32) ([]social.FeedItem, error) {
			if viewer.ID != 0 {
				return social.DashboardFeed(r.Context(), deps, viewer.ID, cursor, limit)
			}
			return social.PublicFeed(r.Context(), deps, cursor, limit)
		})
		if activeTab == "activity" && isExploreFeedFragmentRequest(r) {
			h.renderFeedFragment(w, r, feed, hasNext, nextURL)
			return
		}
		if viewer.ID != 0 {
			var err error
			topRepos, err = social.DashboardRepos(r.Context(), deps, viewer.ID, 30)
			if err != nil && h.logger != nil {
				h.logger.WarnContext(r.Context(), "explore dashboard repos", "error", err)
			}
			viewerOrgs, err = orgsdb.New().ListOrgsForUser(r.Context(), h.pool, viewer.ID)
			if err != nil && h.logger != nil {
				h.logger.WarnContext(r.Context(), "explore org switcher", "error", err)
			}
		}
		var err error
		trendingRepos, err = social.CachedTrendingRepos(r.Context(), deps, social.TrendingScopeWeek, 7, 10)
		if err != nil && h.logger != nil {
			h.logger.WarnContext(r.Context(), "explore trending repos", "error", err)
		}
		trendingUsers, err = social.CachedTrendingUsers(r.Context(), deps, social.TrendingScopeWeek, 7, 8)
		if err != nil && h.logger != nil {
			h.logger.WarnContext(r.Context(), "explore trending users", "error", err)
		}
	}
	if activeTab == "activity" && isExploreFeedFragmentRequest(r) {
		h.renderFeedFragment(w, r, feed, hasNext, nextURL)
		return
	}

	pageHeading := title
	feedHeading := "Public activity"
	emptyTitle := "No public activity yet"
	emptyBody := "Public stars, forks, pushes, issues, pull requests, and follows will appear here."
	if viewer.ID != 0 {
		if activeTab == "activity" {
			pageHeading = "Home"
		}
		feedHeading = "Feed"
		emptyTitle = "Follow people and organizations to build your feed"
		emptyBody = "Stars, forks, pushes, issues, pull requests, and follows from your network will appear here."
	}

	data := map[string]any{
		"Title":          title,
		"ActiveTab":      activeTab,
		"PageHeading":    pageHeading,
		"FeedHeading":    feedHeading,
		"FeedEmptyTitle": emptyTitle,
		"FeedEmptyBody":  emptyBody,
		"Feed":           feed,
		"FeedHasNext":    hasNext,
		"FeedNextURL":    nextURL,
		"TopRepos":       topRepos,
		"ViewerOrgs":     viewerOrgs,
		"TrendingRepos":  trendingRepos,
		"TrendingUsers":  trendingUsers,
		"Path":           path,
		"UseHTMX":        true,
	}
	if err := h.render.RenderPage(w, r, "explore/index", data); err != nil {
		if h.logger != nil {
			h.logger.Error("render explore", "error", err)
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h exploreHandler) renderFeedFragment(w http.ResponseWriter, r *http.Request, feed []social.FeedItem, hasNext bool, nextURL string) {
	data := map[string]any{
		"Feed":        feed,
		"FeedHasNext": hasNext,
		"FeedNextURL": nextURL,
	}
	if err := h.render.RenderFragment(w, "explore/feed_page", data); err != nil {
		if h.logger != nil {
			h.logger.ErrorContext(r.Context(), "render explore feed fragment", "error", err)
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func isExploreFeedFragmentRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.URL.Query().Get("before") != ""
}

func feedPageFor(r *http.Request, load func(social.FeedCursor, int32) ([]social.FeedItem, error)) ([]social.FeedItem, bool, string) {
	items, err := load(parseFeedCursor(r), feedDisplayLimit+1)
	if err != nil {
		return nil, false, ""
	}
	if int32(len(items)) <= feedDisplayLimit {
		return items, false, ""
	}
	display := items[:feedDisplayLimit]
	return display, true, feedNextURL(r, display[len(display)-1])
}

func parseFeedCursor(r *http.Request) social.FeedCursor {
	raw := r.URL.Query().Get("before")
	if raw == "" {
		return social.FeedCursor{}
	}
	parts := strings.SplitN(raw, "~", 2)
	if len(parts) != 2 {
		return social.FeedCursor{}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return social.FeedCursor{}
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return social.FeedCursor{}
	}
	return social.FeedCursor{BeforeCreatedAt: createdAt, BeforeID: id}
}

func feedNextURL(r *http.Request, item social.FeedItem) string {
	q := r.URL.Query()
	q.Set("before", item.CreatedAt.UTC().Format(time.RFC3339Nano)+"~"+strconv.FormatInt(item.ID, 10))
	return r.URL.Path + "?" + q.Encode()
}
