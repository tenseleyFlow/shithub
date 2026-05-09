// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notifications wires the S29 notification web surface:
//
//   - GET  /notifications                       inbox
//   - POST /notifications/{id}/read             mark one read
//   - POST /notifications/{id}/unread           mark one unread
//   - POST /notifications/mark-all-read         clear unread for the viewer
//   - POST /threads/{kind}/{id}/subscribe       subscribe (or override-ignore)
//   - POST /threads/{kind}/{id}/unsubscribe     unsubscribe (per-thread)
//   - GET  /notifications/unsubscribe           one-click HMAC-signed unsub
package notifications

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/notif"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the handler set.
type Deps struct {
	Logger         *slog.Logger
	Render         *render.Renderer
	Pool           *pgxpool.Pool
	UnsubscribeKey []byte
}

// Handlers groups the notification surface handlers. Construct via New.
type Handlers struct {
	d Deps
}

// pageSize bounds inbox pagination. Same default as the search inbox.
const pageSize = 25

// New constructs the handler set.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("notifications: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("notifications: nil Pool")
	}
	return &Handlers{d: d}, nil
}

// MountAuthed registers the routes that REQUIRE a logged-in viewer.
// Mounted from server.go inside a RequireUser-wrapped group.
func (h *Handlers) MountAuthed(r chi.Router) {
	r.Get("/notifications", h.list)
	r.Post("/notifications/{id}/read", h.markRead)
	r.Post("/notifications/{id}/unread", h.markUnread)
	r.Post("/notifications/mark-all-read", h.markAllRead)
	r.Post("/threads/{kind}/{id}/subscribe", h.subscribe)
	r.Post("/threads/{kind}/{id}/unsubscribe", h.unsubscribe)
}

// MountPublic registers the unauthenticated one-click unsubscribe
// endpoint. The HMAC-signed URL embeds the recipient ID + thread
// reference + signature so we can verify without a session cookie
// (RFC 8058 mailers pop links from arbitrary clients).
func (h *Handlers) MountPublic(r chi.Router) {
	r.Get("/notifications/unsubscribe", h.unsubscribeViaToken)
}

// ─── handlers ──────────────────────────────────────────────────────

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next=/notifications", http.StatusSeeOther)
		return
	}
	page := pageFromRequest(r)
	onlyUnread := r.URL.Query().Get("filter") == "unread"

	q := notifdb.New()
	rows, err := q.ListNotificationsForRecipient(r.Context(), h.d.Pool, notifdb.ListNotificationsForRecipientParams{
		RecipientUserID: viewer.ID,
		Column2:         onlyUnread,
		Limit:           int32(pageSize),
		Offset:          int32((page - 1) * pageSize),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notifications: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	unreadCount, _ := q.CountUnreadForRecipient(r.Context(), h.d.Pool, viewer.ID)

	data := map[string]any{
		"Title":         "Notifications",
		"Notifications": rows,
		"UnreadCount":   unreadCount,
		"Filter":        r.URL.Query().Get("filter"),
		"Page":          page,
		"HasPrev":       page > 1,
		"HasNext":       len(rows) == pageSize,
	}
	if err := h.d.Render.RenderPage(w, r, "notifications/inbox", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notifications: render", "error", err)
	}
}

func (h *Handlers) markRead(w http.ResponseWriter, r *http.Request) {
	h.setRead(w, r, true)
}

func (h *Handlers) markUnread(w http.ResponseWriter, r *http.Request) {
	h.setRead(w, r, false)
}

// setRead toggles the unread flag on a single inbox row. The DB path
// owns the recipient_user_id check (the SET … WHERE … filters by both
// id and recipient), so a forged id from another user becomes a
// silent no-op rather than an authoritative 403 — matches the
// existence-leak posture used elsewhere.
func (h *Handlers) setRead(w http.ResponseWriter, r *http.Request, read bool) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	q := notifdb.New()
	if read {
		err = q.SetNotificationRead(r.Context(), h.d.Pool, notifdb.SetNotificationReadParams{
			ID: id, RecipientUserID: viewer.ID,
		})
	} else {
		err = q.SetNotificationUnread(r.Context(), h.d.Pool, notifdb.SetNotificationUnreadParams{
			ID: id, RecipientUserID: viewer.ID,
		})
	}
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "notifications: set read",
			"id", id, "read", read, "error", err)
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

func (h *Handlers) markAllRead(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	if err := notifdb.New().MarkAllReadForRecipient(r.Context(), h.d.Pool, viewer.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "notifications: mark-all-read", "error", err)
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

func (h *Handlers) subscribe(w http.ResponseWriter, r *http.Request) {
	h.threadAction(w, r, true)
}

func (h *Handlers) unsubscribe(w http.ResponseWriter, r *http.Request) {
	h.threadAction(w, r, false)
}

// threadAction toggles per-thread subscription. `subscribe=true`
// inserts (or updates) a row with subscribed=true; `false` flips it
// off. The fan-out worker honors the explicit row over the auto-sub
// derivation.
func (h *Handlers) threadAction(w http.ResponseWriter, r *http.Request, subscribed bool) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	kindStr := chi.URLParam(r, "kind")
	if kindStr != "issue" && kindStr != "pr" {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	reason := "manual"
	if !subscribed {
		reason = "manual_unsubscribe"
	}
	if err := notifdb.New().UpsertNotificationThread(r.Context(), h.d.Pool, notifdb.UpsertNotificationThreadParams{
		RecipientUserID: viewer.ID,
		ThreadKind:      notifdb.NotificationThreadKind(kindStr),
		ThreadID:        id,
		Subscribed:      subscribed,
		Reason:          reason,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "notifications: thread action",
			"kind", kindStr, "id", id, "subscribed", subscribed, "error", err)
	}
	// Bounce back to the thread the viewer was on. Best-effort: when
	// the Referer is missing, fall back to /notifications.
	dest := r.Header.Get("Referer")
	if dest == "" {
		dest = "/notifications"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// unsubscribeViaToken handles the email's one-click List-Unsubscribe
// link. The URL embeds (recipient, thread_kind, thread_id, sig) so
// no session is required. We re-derive the HMAC and compare in
// constant time; a mismatch returns 400.
func (h *Handlers) unsubscribeViaToken(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uStr, tk, tiStr, sig := q.Get("u"), q.Get("tk"), q.Get("ti"), q.Get("sig")
	uid, err := strconv.ParseInt(uStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	tid, err := strconv.ParseInt(tiStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if !notif.VerifyUnsubscribe(h.d.UnsubscribeKey, uid, tk, tid, sig) {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if tk != "issue" && tk != "pr" {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if err := notifdb.New().UpsertNotificationThread(r.Context(), h.d.Pool, notifdb.UpsertNotificationThreadParams{
		RecipientUserID: uid,
		ThreadKind:      notifdb.NotificationThreadKind(tk),
		ThreadID:        tid,
		Subscribed:      false,
		Reason:          "email_one_click",
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "notifications: token unsubscribe",
			"recipient", uid, "kind", tk, "id", tid, "error", err)
	}
	if err := h.d.Render.RenderPage(w, r, "notifications/unsubscribed", map[string]any{
		"Title": "Unsubscribed",
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "notifications: render unsub", "error", err)
	}
}

// pageFromRequest pulls ?page=N, defaulting to 1 on missing/invalid.
func pageFromRequest(r *http.Request) int {
	p := r.URL.Query().Get("page")
	if p == "" {
		return 1
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 10000 {
		return 1
	}
	return n
}
