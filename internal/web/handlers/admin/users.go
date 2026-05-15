// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// usersList renders the paginated, filterable user list.
func (h *Handlers) usersList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	suspendedOnly := q.Get("suspended") == "1"
	deletedOnly := q.Get("deleted") == "1"
	const perPage = 50
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	rows, _ := h.aq.ListUsersForAdmin(r.Context(), h.d.Pool, admindb.ListUsersForAdminParams{
		UsernamePrefix: pgtype.Text{String: prefix, Valid: prefix != ""},
		SuspendedOnly:  pgtype.Bool{Bool: true, Valid: suspendedOnly},
		DeletedOnly:    pgtype.Bool{Bool: true, Valid: deletedOnly},
		Limit:          perPage,
		Offset:         int32((page - 1) * perPage),
	})
	h.d.Render.RenderPage(w, r, "admin/users_list", map[string]any{
		"Title":         "Users",
		"CSRFToken":     middleware.CSRFTokenForRequest(r),
		"AdminActive":   "users",
		"Users":         rows,
		"Q":             prefix,
		"SuspendedOnly": suspendedOnly,
		"DeletedOnly":   deletedOnly,
		"Page":          page,
		"NextPage":      page + 1,
		"PrevPage":      page - 1,
		"HasMore":       len(rows) == perPage,
		"Notice":        adminNotice(q.Get("notice")),
	})
}

// userView renders the per-user admin page with action buttons.
func (h *Handlers) userView(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	notice := r.URL.Query().Get("notice")
	h.d.Render.RenderPage(w, r, "admin/user_view", map[string]any{
		"Title":       user.Username + " · admin",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "users",
		"User":        user,
		"Notice":      adminNotice(notice),
	})
}

// userSuspend flips the suspended_at timestamp + emits an audit row.
// The user's existing sessions remain valid — they just hit policy
// gates that deny writes — but the login page surfaces the reason on
// the next sign-in attempt.
func (h *Handlers) userSuspend(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		reason = "suspended by site admin"
	}
	if err := h.uq.SuspendUser(r.Context(), h.d.Pool, usersdb.SuspendUserParams{
		ID:              user.ID,
		SuspendedReason: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin: suspend", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminUserSuspended, audit.TargetUser, user.ID,
		map[string]any{"reason": reason})
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(user.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// userUnsuspend clears the suspension.
func (h *Handlers) userUnsuspend(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	if err := h.uq.SuspendUser(r.Context(), h.d.Pool, usersdb.SuspendUserParams{
		ID:              user.ID,
		SuspendedReason: pgtype.Text{Valid: false},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin: unsuspend", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	// SR2 M2: was inline SQL with the comment "lean on a one-off SQL
	// exec rather than extending the users sqlc surface." That comment
	// stopped being true the moment we needed to test it; usersdb has
	// the UnsuspendUser query now.
	if err := h.uq.UnsuspendUser(r.Context(), h.d.Pool, user.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin: unsuspend clear", "error", err)
	}
	h.recordAdminAction(r, audit.ActionAdminUserUnsuspended, audit.TargetUser, user.ID, nil)
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(user.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// userToggleSiteAdmin grants or revokes the is_site_admin flag.
// Defense in depth: an admin can't revoke their own flag (would lock
// themselves out of the surface).
func (h *Handlers) userToggleSiteAdmin(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if user.ID == viewer.ID && user.IsSiteAdmin {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "you can't revoke your own admin flag — ask another admin")
		return
	}
	to := !user.IsSiteAdmin
	if err := h.aq.SetUserSiteAdmin(r.Context(), h.d.Pool, admindb.SetUserSiteAdminParams{
		ID: user.ID, IsSiteAdmin: to,
	}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	action := audit.ActionAdminSiteAdminGranted
	if !to {
		action = audit.ActionAdminSiteAdminRevoked
	}
	h.recordAdminAction(r, action, audit.TargetUser, user.ID, nil)
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(user.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// userResetPassword mints a one-time reset token and emails it. Same
// machinery the public reset flow uses; admin path skips the throttle
// because the operator is the rate-limiter here.
func (h *Handlers) userResetPassword(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	if !user.PrimaryEmailID.Valid {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "user has no primary email")
		return
	}
	em, err := h.uq.GetUserEmailByID(r.Context(), h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin reset: primary email lookup", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	tokEnc, tokHash, err := token.New()
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin reset: token mint", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := h.uq.CreatePasswordReset(r.Context(), h.d.Pool, usersdb.CreatePasswordResetParams{
		UserID: user.ID, TokenHash: tokHash, ExpiresAt: expires,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "admin reset: create row", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Send the message (best-effort: the token row is committed; the
	// admin can resend by clicking again). The audit row records
	// whether the send succeeded, so a stuck mailbox shows up in
	// /admin/audit instead of being invisible (SR2 C3 fix).
	emailSent := false
	emailErr := ""
	if h.d.Email != nil {
		msg, err := email.ResetMessage(h.d.Branding, string(em.Email), tokEnc)
		if err != nil {
			emailErr = err.Error()
			h.d.Logger.WarnContext(r.Context(), "admin reset: build message", "error", err)
		} else if err := h.d.Email.Send(r.Context(), msg); err != nil {
			emailErr = err.Error()
			h.d.Logger.WarnContext(r.Context(), "admin reset: send", "error", err)
		} else {
			emailSent = true
		}
	} else {
		emailErr = "no sender wired"
	}
	auditMeta := map[string]any{"email": string(em.Email), "email_sent": emailSent}
	if emailErr != "" {
		auditMeta["email_error"] = emailErr
	}
	h.recordAdminAction(r, audit.ActionAdminUserPasswordReset, audit.TargetUser, user.ID, auditMeta)
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(user.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// loadUser parses the {id} param and fetches the row. 404s on miss.
func (h *Handlers) loadUser(w http.ResponseWriter, r *http.Request) (usersdb.User, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "bad id")
		return usersdb.User{}, false
	}
	user, err := h.uq.GetUserIncludingDeleted(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return usersdb.User{}, false
	}
	return user, true
}
