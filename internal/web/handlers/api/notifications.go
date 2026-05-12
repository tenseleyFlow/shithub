// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountNotifications registers the S50 §14 notifications REST surface.
//
//	GET   /api/v1/notifications[?all=true|false&page=&per_page=]
//	PUT   /api/v1/notifications                       mark all read
//	GET   /api/v1/notifications/threads/{id}          single fetch
//	PATCH /api/v1/notifications/threads/{id}          mark read/unread
//
// Mirrors GitHub's notifications API (where each "thread" is a single
// `notifications` row in our schema). Scope: `user:read` for the
// reading endpoints, `user:write` for the mutations. The PAT auth
// middleware already ensures `auth.UserID != 0`; everything here
// scopes to that user — no cross-user reads/writes possible.
func (h *Handlers) mountNotifications(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/notifications", h.notificationsList)
		r.Get("/api/v1/notifications/threads/{id}", h.notificationGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Put("/api/v1/notifications", h.notificationsMarkAllRead)
		r.Patch("/api/v1/notifications/threads/{id}", h.notificationPatch)
	})
}

type notificationResponse struct {
	ID          int64           `json:"id"`
	Unread      bool            `json:"unread"`
	Reason      string          `json:"reason"`
	Kind        string          `json:"kind"`
	UpdatedAt   string          `json:"updated_at"`
	LastEventAt string          `json:"last_event_at"`
	Subject     subjectResponse `json:"subject"`
	Repository  *repoStub       `json:"repository,omitempty"`
}

type subjectResponse struct {
	Title  string `json:"title,omitempty"`
	Type   string `json:"type"` // "issue" | "pull_request" | "commit" | ...
	Number int64  `json:"number,omitempty"`
}

type repoStub struct {
	OwnerLogin string `json:"owner_login"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
}

func presentNotification(row notifdb.ListNotificationsForRecipientRow) notificationResponse {
	out := notificationResponse{
		ID:          row.ID,
		Unread:      row.Unread,
		Reason:      row.Reason,
		Kind:        row.Kind,
		UpdatedAt:   row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		LastEventAt: row.LastEventAt.Time.UTC().Format(time.RFC3339),
		Subject: subjectResponse{
			Title:  row.ThreadTitle,
			Type:   threadKindToSubjectType(row.ThreadKind),
			Number: row.ThreadNumber,
		},
	}
	if row.RepoOwnerUsername != "" && row.RepoName != "" {
		out.Repository = &repoStub{
			OwnerLogin: row.RepoOwnerUsername,
			Name:       row.RepoName,
			FullName:   row.RepoOwnerUsername + "/" + row.RepoName,
		}
	}
	return out
}

// threadKindToSubjectType maps the notification thread enum to the
// GitHub-style `subject.type` string. Returns "" for threadless
// notifications (the row's `thread_kind` is NULL).
func threadKindToSubjectType(k notifdb.NullNotificationThreadKind) string {
	if !k.Valid {
		return ""
	}
	switch k.NotificationThreadKind {
	case notifdb.NotificationThreadKindIssue:
		return "issue"
	case notifdb.NotificationThreadKindPr:
		return "pull_request"
	}
	return string(k.NotificationThreadKind)
}

func (h *Handlers) notificationsList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	// GitHub's convention: `all=true` includes read notifications.
	// Default (no param or `all=false`) returns unread only.
	allParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("all")))
	onlyUnread := allParam != "true"

	q := notifdb.New()
	total, err := q.CountNotificationsForRecipient(r.Context(), h.d.Pool, notifdb.CountNotificationsForRecipientParams{
		RecipientUserID: auth.UserID,
		Column2:         onlyUnread,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count notifications", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListNotificationsForRecipient(r.Context(), h.d.Pool, notifdb.ListNotificationsForRecipientParams{
		RecipientUserID: auth.UserID,
		Column2:         onlyUnread,
		Limit:           int32(perPage),
		Offset:          int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list notifications", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]notificationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentNotification(row))
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) notificationsMarkAllRead(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if err := notifdb.New().MarkAllReadForRecipient(r.Context(), h.d.Pool, auth.UserID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: mark all read", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "mark failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) notificationGet(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}
	q := notifdb.New()
	n, err := q.GetNotification(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "notification not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get notification", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	// Cross-recipient probes return 404 (no existence leak).
	if n.RecipientUserID != auth.UserID {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}
	// Bring up to list-shape with the same JOIN data. Cheapest path
	// is to re-list with a tight filter; since GetNotification ships
	// a bare row, materialise just the subject fields directly here.
	out := notificationResponse{
		ID:          n.ID,
		Unread:      n.Unread,
		Reason:      n.Reason,
		Kind:        n.Kind,
		UpdatedAt:   n.UpdatedAt.Time.UTC().Format(time.RFC3339),
		LastEventAt: n.LastEventAt.Time.UTC().Format(time.RFC3339),
		Subject:     subjectResponse{Type: threadKindToSubjectType(n.ThreadKind)},
	}
	writeJSON(w, http.StatusOK, out)
}

type notificationPatchRequest struct {
	// Unread, when non-nil, sets the read state. `false` marks the
	// notification read (matches gh's PATCH which only flips to
	// read); `true` marks it unread again (a shithub extension).
	Unread *bool `json:"unread"`
}

func (h *Handlers) notificationPatch(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}
	q := notifdb.New()
	// Confirm ownership before mutating — the update queries already
	// scope by recipient_user_id, but checking up front lets us
	// return the right status code (404 vs 204-on-noop).
	n, err := q.GetNotification(r.Context(), h.d.Pool, id)
	if err != nil || n.RecipientUserID != auth.UserID {
		writeAPIError(w, http.StatusNotFound, "notification not found")
		return
	}
	// Empty body / nil Unread → treat as "mark read" (gh's default
	// PATCH semantics).
	want := false
	if r.ContentLength > 0 {
		var body notificationPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Unread != nil {
			want = *body.Unread
		}
	}
	if want {
		err = q.SetNotificationUnread(r.Context(), h.d.Pool, notifdb.SetNotificationUnreadParams{
			ID: id, RecipientUserID: auth.UserID,
		})
	} else {
		err = q.SetNotificationRead(r.Context(), h.d.Pool, notifdb.SetNotificationReadParams{
			ID: id, RecipientUserID: auth.UserID,
		})
	}
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: patch notification", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "mark failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
