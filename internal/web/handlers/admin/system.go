// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"runtime"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// systemPage surfaces operator-level introspection: shithubd version,
// Go runtime, DB connection counts, repo storage usage. Not a Grafana
// replacement; the spec calls it out as "link out instead" for deep
// monitoring. This is just the at-a-glance "is the box healthy" view.
func (h *Handlers) systemPage(w http.ResponseWriter, r *http.Request) {
	pool := h.d.Pool
	stat := pool.Stat()

	// Cheap aggregates from pg_stat_database for the connected DB.
	var dbName, dbUser string
	var connCount int64
	_ = pool.QueryRow(r.Context(),
		`SELECT current_database(), current_user, COUNT(*)::bigint
		   FROM pg_stat_activity
		  WHERE datname = current_database()`).Scan(&dbName, &dbUser, &connCount)

	// Repo disk usage rollup (sum of repos.disk_used_bytes; computed
	// by the repo:size_recalc job, kept warm there).
	var repoBytes pgxBytesRow
	_ = pool.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(disk_used_bytes), 0)::bigint, COUNT(*)::bigint
		   FROM repos WHERE deleted_at IS NULL`).Scan(&repoBytes.Sum, &repoBytes.Count)

	h.d.Render.RenderPage(w, r, "admin/system", map[string]any{
		"Title":       "System",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "system",
		"Version":     h.d.Version,
		"GoVersion":   runtime.Version(),
		"GoOS":        runtime.GOOS,
		"GoArch":      runtime.GOARCH,
		"Pool": map[string]any{
			"acquired":     stat.AcquiredConns(),
			"idle":         stat.IdleConns(),
			"total":        stat.TotalConns(),
			"max":          stat.MaxConns(),
			"acquireCount": stat.AcquireCount(),
		},
		"DB": map[string]any{
			"name":  dbName,
			"user":  dbUser,
			"conns": connCount,
		},
		"Repos": map[string]any{
			"diskBytes": repoBytes.Sum,
			"count":     repoBytes.Count,
		},
	})
}

// pgxBytesRow is a tiny scan target.
type pgxBytesRow struct {
	Sum   int64
	Count int64
}

// emailQueue surfaces the recent transactional + notification email
// log. Filter chips for status defer to a follow-up sprint; for now
// we show the last 100 transactional rows.
func (h *Handlers) emailQueue(w http.ResponseWriter, r *http.Request) {
	rows, _ := h.aq.ListRecentTransactionalEmails(r.Context(), h.d.Pool, h.recentEmailsParams())
	h.d.Render.RenderPage(w, r, "admin/email", map[string]any{
		"Title":       "Email queue",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "email",
		"Rows":        rows,
	})
}
