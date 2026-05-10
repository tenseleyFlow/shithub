// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// recordAdminAction writes an audit row attributed to the current
// site admin, with optional impersonation-target metadata so post-
// incident forensics has both the real actor and the impersonated id.
//
// The (actorID, meta) substitution lives on middleware.CurrentUser
// so non-admin handlers can record uniform impersonation trails
// (SR2 H2).
func (h *Handlers) recordAdminAction(r *http.Request, action audit.Action, target audit.Target, targetID int64, meta map[string]any) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	actorID, meta := viewer.AuditActor(meta)
	if err := h.d.Audit.Record(r.Context(), h.d.Pool, actorID, action, target, targetID, meta); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(r.Context(), "admin: audit write", "error", err, "action", string(action))
	}
}

// adminNotice maps short query-string codes to user-visible strings.
func adminNotice(code string) string {
	switch code {
	case "saved":
		return "Saved."
	}
	return ""
}

// recentEmailsParams supplies the default ListRecentTransactionalEmails
// args (last 100, no status filter). Kept as a helper so future pages
// can call it from multiple places without duplicating the literal.
func (h *Handlers) recentEmailsParams() admindb.ListRecentTransactionalEmailsParams {
	return admindb.ListRecentTransactionalEmailsParams{Limit: 100}
}
