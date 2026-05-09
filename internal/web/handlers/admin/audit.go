// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// auditList renders the filterable audit log.
func (h *Handlers) auditList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	const perPage = 100
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	params := admindb.ListAuditForAdminParams{
		Limit:  perPage,
		Offset: int32((page - 1) * perPage),
	}
	if v := strings.TrimSpace(q.Get("actor")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			params.ActorID = pgtype.Int8{Int64: id, Valid: true}
		}
	}
	if v := strings.TrimSpace(q.Get("action")); v != "" {
		params.ActionPrefix = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(q.Get("target_type")); v != "" {
		params.TargetType = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(q.Get("target_id")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			params.TargetID = pgtype.Int8{Int64: id, Valid: true}
		}
	}
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			params.Since = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	if v := strings.TrimSpace(q.Get("until")); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			params.Until = pgtype.Timestamptz{Time: t.AddDate(0, 0, 1), Valid: true}
		}
	}
	rows, _ := h.aq.ListAuditForAdmin(r.Context(), h.d.Pool, params)
	h.d.Render.RenderPage(w, r, "admin/audit_list", map[string]any{
		"Title":       "Audit log",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "audit",
		"Rows":        rows,
		"Filters":     q.Encode(),
		"Q":           q,
		"Page":        page,
		"NextPage":    page + 1,
		"PrevPage":    page - 1,
		"HasMore":     len(rows) == perPage,
	})
}
