// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/social"
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
	var (
		feed          []social.FeedItem
		hasNext       bool
		nextURL       string
		trendingRepos []social.TrendingRepo
		trendingUsers []social.TrendingUser
	)
	if h.pool != nil {
		deps := social.Deps{Pool: h.pool, Logger: h.logger}
		feed, hasNext, nextURL = feedPageFor(r, func(cursor social.FeedCursor, limit int32) ([]social.FeedItem, error) {
			return social.PublicFeed(r.Context(), deps, cursor, limit)
		})
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

	data := map[string]any{
		"Title":         title,
		"ActiveTab":     activeTab,
		"Feed":          feed,
		"FeedHasNext":   hasNext,
		"FeedNextURL":   nextURL,
		"TrendingRepos": trendingRepos,
		"TrendingUsers": trendingUsers,
		"Path":          path,
	}
	if err := h.render.RenderPage(w, r, "explore/index", data); err != nil {
		if h.logger != nil {
			h.logger.Error("render explore", "error", err)
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
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
