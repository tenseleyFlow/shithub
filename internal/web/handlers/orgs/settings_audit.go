// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const orgAuditLogPerPage = 100

func (h *Handlers) settingsAuditLog(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	params := usersdb.ListOrgAuditLogParams{
		OrgID:       org.ID,
		LimitCount:  orgAuditLogPerPage,
		OffsetCount: int32((page - 1) * orgAuditLogPerPage),
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
	rows, err := usersdb.New().ListOrgAuditLog(r.Context(), h.d.Pool, params)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org audit log: list", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.d.Render.RenderPage(w, r, "orgs/settings_audit", map[string]any{
		"Title":             org.Slug + " · Audit log",
		"Org":               org,
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"OrgSettingsActive": "audit",
		"BillingEnabled":    h.billingConfigured(),
		"Rows":              rows,
		"Filters":           orgAuditFilters(q),
		"Q":                 q,
		"Page":              page,
		"NextPage":          page + 1,
		"PrevPage":          page - 1,
		"HasMore":           len(rows) == orgAuditLogPerPage,
	})
}

func orgAuditFilters(q url.Values) string {
	clone := make(url.Values, len(q))
	for k, values := range q {
		if k == "page" {
			continue
		}
		cp := make([]string, len(values))
		copy(cp, values)
		clone[k] = cp
	}
	return clone.Encode()
}
