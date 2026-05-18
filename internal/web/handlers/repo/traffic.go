// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) recordRepoView(r *http.Request, row reposdb.Repo, owner string) {
	event := repotraffic.Event{
		RepoID:       row.ID,
		OccurredAt:   time.Now(),
		VisitorKey:   repoTrafficVisitorKey(r),
		Path:         repoTrafficPath(r, owner, row.Name),
		ReferrerHost: repotraffic.ExternalReferrerHost(r.Header.Get("Referer"), r.Host),
	}
	if err := repotraffic.RecordView(r.Context(), h.d.Pool, event); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(r.Context(), "repo traffic: record view", "repo_id", row.ID, "error", err)
	}
}

func repoTrafficVisitorKey(r *http.Request) string {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !viewer.IsAnonymous() {
		return "user:" + strconv.FormatInt(viewer.ID, 10)
	}
	return "anon:" + repoRequestIP(r) + "|" + r.UserAgent()
}

func repoTrafficPath(r *http.Request, owner, repoName string) string {
	prefix := "/" + owner + "/" + repoName
	if strings.HasPrefix(r.URL.Path, prefix) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if rest == "" {
			return "/"
		}
		return rest
	}
	return r.URL.Path
}

func repoRequestIP(r *http.Request) string {
	if ip := middleware.RealIPFromContext(r.Context(), r); ip != "" {
		return ip
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
