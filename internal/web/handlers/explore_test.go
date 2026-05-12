// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/social"
)

func TestExploreFeedFragmentRequestRequiresHTMXAndCursor(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/explore?before=2026-05-12T00:00:00Z~42", nil)
	if isExploreFeedFragmentRequest(req) {
		t.Fatal("plain cursor request should stay a full-page no-JS fallback")
	}

	req.Header.Set("HX-Request", "true")
	if !isExploreFeedFragmentRequest(req) {
		t.Fatal("HTMX cursor request should render feed fragment")
	}

	req = httptest.NewRequest("GET", "/explore", nil)
	req.Header.Set("HX-Request", "true")
	if isExploreFeedFragmentRequest(req) {
		t.Fatal("HTMX first-page request should not render feed fragment")
	}
}

func TestFeedPageForBuildsNextCursorFromLastDisplayedItem(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/explore?tab=activity", nil)
	base := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	items := make([]social.FeedItem, 0, feedDisplayLimit+1)
	for i := int32(0); i < feedDisplayLimit+1; i++ {
		items = append(items, social.FeedItem{
			ID:        int64(1000 + i),
			CreatedAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}

	got, hasNext, nextURL := feedPageFor(req, func(cursor social.FeedCursor, limit int32) ([]social.FeedItem, error) {
		if !cursor.BeforeCreatedAt.IsZero() || cursor.BeforeID != 0 {
			t.Fatalf("first page cursor = %+v, want zero", cursor)
		}
		if limit != feedDisplayLimit+1 {
			t.Fatalf("limit = %d, want %d", limit, feedDisplayLimit+1)
		}
		return items, nil
	})

	if len(got) != int(feedDisplayLimit) {
		t.Fatalf("display item count = %d, want %d", len(got), feedDisplayLimit)
	}
	if !hasNext {
		t.Fatal("hasNext = false, want true")
	}
	wantCursor := got[len(got)-1].CreatedAt.UTC().Format(time.RFC3339Nano) + "~" + "1019"
	parsed, err := url.Parse(nextURL)
	if err != nil {
		t.Fatalf("parse nextURL: %v", err)
	}
	if parsed.Query().Get("tab") != "activity" || parsed.Query().Get("before") != wantCursor {
		t.Fatalf("nextURL = %q, want preserved query and cursor %q", nextURL, wantCursor)
	}
}
