// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
)

func dropLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOptionalUser_PopulatesIsSuspended is the audit's regression
// guard for finding C1: when the underlying user record carries a
// non-NULL `suspended_at` (relayed by the lookup as IsSuspended=true),
// the CurrentUser bound into context must reflect it. Every handler
// that constructs a policy.UserActor reads `viewer.IsSuspended`
// downstream — without propagation here, the suspension gate is
// silently bypassed.
func TestOptionalUser_PopulatesIsSuspended(t *testing.T) {
	t.Parallel()

	bind := func(t *testing.T, suspended bool) CurrentUser {
		t.Helper()
		var captured CurrentUser
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = CurrentUserFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		lookup := func(ctx context.Context, id int64) (UserLookupResult, error) {
			return UserLookupResult{
				Username: "alice", SessionEpoch: 7, IsSuspended: suspended,
			}, nil
		}
		// Inject the session directly into context — bypasses the
		// SessionLoader middleware (no cookie store needed for this
		// unit test of the lookup-to-context plumbing).
		s := &session.Session{UserID: 42, Epoch: 7}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), sessionKey, s))
		rec := httptest.NewRecorder()
		OptionalUser(lookup)(next).ServeHTTP(rec, req)
		return captured
	}

	if u := bind(t, true); !u.IsSuspended {
		t.Errorf("suspended=true: got IsSuspended=false, want true")
	}
	if u := bind(t, false); u.IsSuspended {
		t.Errorf("suspended=false: got IsSuspended=true, want false")
	}
}

// TestOptionalUser_StaleEpochSkipsBind is the corollary: when the
// recorded session epoch doesn't match the current users.session_epoch
// (because the user logged out everywhere), the binding is skipped so
// downstream RequireUser bounces them to /login. This existed before
// the C1 fix; covering it here pins the contract.
func TestOptionalUser_StaleEpochSkipsBind(t *testing.T) {
	t.Parallel()
	var captured CurrentUser
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = CurrentUserFromContext(r.Context())
	})
	lookup := func(ctx context.Context, id int64) (UserLookupResult, error) {
		return UserLookupResult{Username: "alice", SessionEpoch: 99}, nil
	}
	s := &session.Session{UserID: 42, Epoch: 7}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), sessionKey, s))
	OptionalUser(lookup)(next).ServeHTTP(httptest.NewRecorder(), req)
	if !captured.IsAnonymous() {
		t.Errorf("stale epoch: got CurrentUser{ID=%d}, want anonymous", captured.ID)
	}
}

func TestRequestID_AssignsAndEchoes(t *testing.T) {
	t.Parallel()
	var captured string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if captured == "" {
		t.Errorf("RequestID middleware did not assign id")
	}
	if got := rec.Header().Get(RequestIDHeader); got != captured {
		t.Errorf("response header: got %q, want %q", got, captured)
	}
}

func TestRequestID_AcceptsValidIncoming(t *testing.T) {
	t.Parallel()
	const valid = "abc123-trace_id"
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := RequestIDFromContext(r.Context()); got != valid {
			t.Errorf("got %q, want %q", got, valid)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, valid)
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestRequestID_RejectsMalformedIncoming(t *testing.T) {
	t.Parallel()
	const evil = "abc<script>"
	var captured string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, evil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured == evil || captured == "" {
		t.Errorf("malformed inbound id was accepted: %q", captured)
	}
}

func TestRecover_RendersHandlerWithRequestID(t *testing.T) {
	t.Parallel()
	pinged := struct {
		called bool
		reqID  string
	}{}
	ph := panicHandlerFunc(func(_ http.ResponseWriter, _ *http.Request, requestID string, _ any) {
		pinged.called = true
		pinged.reqID = requestID
	})
	stack := RequestID(Recover(dropLogger(), ph)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !pinged.called {
		t.Fatalf("panic handler was not invoked")
	}
	if pinged.reqID == "" {
		t.Errorf("panic handler received empty request id")
	}
}

func TestRecover_LetsAbortHandlerThrough(t *testing.T) {
	t.Parallel()
	stack := Recover(dropLogger(), nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("expected http.ErrAbortHandler to escape Recover, got %v", r)
		}
	}()
	stack.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRecover_FallbackBodyIncludesRequestID(t *testing.T) {
	t.Parallel()
	stack := RequestID(Recover(dropLogger(), nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("kapow")
	})))
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request_id=") {
		t.Errorf("body missing request_id reference: %q", rec.Body.String())
	}
}

func TestSecureHeaders_StampsExpectedSet(t *testing.T) {
	t.Parallel()
	cfg := DefaultSecureHeaders()
	stack := SecureHeaders(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, header := range []string{
		"Content-Security-Policy",
		"Referrer-Policy",
		"Permissions-Policy",
		"X-Frame-Options",
		"X-Content-Type-Options",
		"Cross-Origin-Opener-Policy",
		"Cross-Origin-Resource-Policy",
	} {
		if rec.Header().Get(header) == "" {
			t.Errorf("missing header %q", header)
		}
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", got)
	}
}

func TestSecureHeaders_HSTSRequiresTLS(t *testing.T) {
	t.Parallel()
	stack := SecureHeaders(DefaultSecureHeaders())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	// Plain HTTP — HSTS must NOT be set.
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Errorf("HSTS leaked on plain HTTP: %q", rec.Header().Get("Strict-Transport-Security"))
	}

	// X-Forwarded-Proto=https — HSTS must be set.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	stack.ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Errorf("HSTS missing under X-Forwarded-Proto=https")
	}
}

func TestTimeout_CancelsContext(t *testing.T) {
	t.Parallel()
	stack := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				t.Errorf("context err: got %v, want DeadlineExceeded", r.Context().Err())
			}
		case <-time.After(time.Second):
			t.Errorf("timeout did not fire")
		}
	}))
	stack.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestCompress_SkipsForNonGzipClients(t *testing.T) {
	t.Parallel()
	stack := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding leaked: %q", rec.Header().Get("Content-Encoding"))
	}
}

func TestCompress_GzipsForCapableClients(t *testing.T) {
	t.Parallel()
	stack := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("a", 4096)))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	stack.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding: got %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
}

// panicHandlerFunc is a tiny adapter that lets table-driven tests pass a
// closure where PanicHandler is expected.
type panicHandlerFunc func(w http.ResponseWriter, r *http.Request, requestID string, recovered any)

func (f panicHandlerFunc) HandlePanic(w http.ResponseWriter, r *http.Request, requestID string, recovered any) {
	f(w, r, requestID, recovered)
}
