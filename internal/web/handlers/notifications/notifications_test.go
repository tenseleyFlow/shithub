// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

func TestNotificationsPageHref(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		filter string
		page   int
		want   string
	}{
		{name: "all first page", want: "/notifications"},
		{name: "all later page", page: 3, want: "/notifications?page=3"},
		{name: "unread first page", filter: "unread", want: "/notifications?filter=unread"},
		{name: "unread later page", filter: "unread", page: 2, want: "/notifications?filter=unread&page=2"},
		{name: "unknown filter drops", filter: "done", page: 1, want: "/notifications"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := notificationsPageHref(tt.filter, tt.page); got != tt.want {
				t.Fatalf("notificationsPageHref(%q, %d) = %q, want %q", tt.filter, tt.page, got, tt.want)
			}
		})
	}
}

func TestSafeNotificationReturnPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{path: "/notifications", want: true},
		{path: "/notifications?filter=unread&page=2", want: true},
		{path: "/settings/notifications", want: false},
		{path: "//evil.test/notifications", want: false},
		{path: "https://evil.test/notifications", want: false},
		{path: "/notifications\r\nLocation: https://evil.test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := safeNotificationReturnPath(tt.path); got != tt.want {
				t.Fatalf("safeNotificationReturnPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestFilterFromRequestSanitizesUnsupportedFilters(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/notifications?filter=done", nil)
	if got := filterFromRequest(req); got != "" {
		t.Fatalf("filterFromRequest(done) = %q, want empty", got)
	}
	req = httptest.NewRequest("GET", "/notifications?filter=unread", nil)
	if got := filterFromRequest(req); got != "unread" {
		t.Fatalf("filterFromRequest(unread) = %q, want unread", got)
	}
}

func TestNotificationInboxItemFromPullRequestRow(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	item := notificationInboxItemFromRow(notifdb.ListNotificationsForRecipientRow{
		ID:                42,
		Kind:              "review_requested",
		Reason:            "review_requested",
		Unread:            true,
		ThreadKind:        notifdb.NullNotificationThreadKind{NotificationThreadKind: notifdb.NotificationThreadKindPr, Valid: true},
		LastEventAt:       pgtype.Timestamptz{Time: when, Valid: true},
		ActorUsername:     "mona",
		RepoOwnerUsername: "octo-org",
		RepoName:          "hello-world",
		ThreadNumber:      17,
		ThreadTitle:       "Add notification parity",
	})

	if item.ThreadURL != "/octo-org/hello-world/pulls/17" {
		t.Fatalf("ThreadURL = %q", item.ThreadURL)
	}
	if item.RepoURL != "/octo-org/hello-world" || item.RepoFullName != "octo-org/hello-world" {
		t.Fatalf("repo link = %q %q", item.RepoURL, item.RepoFullName)
	}
	if item.ActorURL != "/mona" || item.ActorUsername != "mona" {
		t.Fatalf("actor link = %q %q", item.ActorURL, item.ActorUsername)
	}
	if item.Icon != "git-pull-request" || item.StateClass != "pr" {
		t.Fatalf("state = %q %q", item.Icon, item.StateClass)
	}
	if item.KindLabel != "Review requested" || item.ReasonLabel != "Review requested" {
		t.Fatalf("labels = %q %q", item.KindLabel, item.ReasonLabel)
	}
	if !item.LastEventAt.Equal(when) {
		t.Fatalf("LastEventAt = %v, want %v", item.LastEventAt, when)
	}
}
