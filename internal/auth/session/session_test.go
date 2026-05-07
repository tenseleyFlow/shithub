// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *CookieStore {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	store, err := NewCookieStore(CookieStoreConfig{Key: key})
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	return store
}

func TestCookieStore_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	rec := httptest.NewRecorder()
	saved := &Session{UserID: 42, Theme: "dark"}
	saved.AddFlash("welcome back")
	if err := store.Save(rec, httptest.NewRequest(http.MethodGet, "/", nil), saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatalf("expected one %s cookie, got %v", CookieName, cookies)
	}

	// Round-trip via a fresh request carrying the cookie.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	loaded, err := store.Load(req)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.UserID != 42 {
		t.Errorf("UserID: got %d, want 42", loaded.UserID)
	}
	if loaded.Theme != "dark" {
		t.Errorf("Theme: got %q, want dark", loaded.Theme)
	}
	flashes := loaded.PopFlashes()
	if len(flashes) != 1 || flashes[0] != "welcome back" {
		t.Errorf("Flashes: got %v", flashes)
	}
}

func TestCookieStore_TamperedCookieYieldsEmptySession(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	//nolint:gosec // G124: test fixture intentionally constructs a malformed cookie to verify Load tolerates it.
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "this-is-not-a-valid-aead-payload"})
	loaded, err := store.Load(req)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.IsAnonymous() || loaded.UserID != 0 {
		t.Errorf("expected anonymous session, got %+v", loaded)
	}
}

func TestCookieStore_ExpiredSessionYieldsEmpty(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	store.maxAge = 1 * time.Second
	store.clock = func() time.Time { return time.Unix(1_000_000, 0) }

	rec := httptest.NewRecorder()
	if err := store.Save(rec, httptest.NewRequest(http.MethodGet, "/", nil), &Session{UserID: 7}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cookie := rec.Result().Cookies()[0]

	// Advance the clock past expiry.
	store.clock = func() time.Time { return time.Unix(1_000_000+10, 0) }
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	loaded, _ := store.Load(req)
	if !loaded.IsAnonymous() {
		t.Errorf("expected expired session to be anonymous, got %+v", loaded)
	}
}

func TestCookieStore_KeySizeEnforced(t *testing.T) {
	t.Parallel()
	if _, err := NewCookieStore(CookieStoreConfig{Key: []byte("too-short")}); err == nil {
		t.Errorf("expected error for short key")
	}
}

func TestCookieStore_ClearDeletesCookie(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	rec := httptest.NewRecorder()
	store.Clear(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one cookie, got %d", len(cookies))
	}
	if cookies[0].MaxAge >= 0 {
		t.Errorf("Clear cookie MaxAge: got %d, want negative", cookies[0].MaxAge)
	}
	header := rec.Header().Get("Set-Cookie")
	if !strings.Contains(header, CookieName) {
		t.Errorf("Set-Cookie missing %s: %q", CookieName, header)
	}
}
