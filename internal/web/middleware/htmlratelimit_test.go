// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// htmlLimitFixture wires the middleware around a tiny pass-through
// handler that records whether it was reached.
type htmlLimitFixture struct {
	limiter *ratelimit.Limiter
	reached int
	chain   http.Handler
	cfg     HTMLRateLimitConfig
}

type htmlBodyOpts struct {
	acceptHTML bool
}

func newHTMLLimitFixture(t *testing.T, cfg HTMLRateLimitConfig) *htmlLimitFixture {
	t.Helper()
	l := ratelimit.New(dbtest.NewTestDB(t))
	f := &htmlLimitFixture{limiter: l, cfg: cfg}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	f.chain = HTMLRateLimit(l, cfg)(terminal)
	return f
}

// serve makes a request with a CurrentUser injected like
// OptionalUser would. Returns the recorder so the caller can
// inspect status + headers + body.
func (f *htmlLimitFixture) serve(viewer CurrentUser, opts htmlBodyOpts) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/somerepo/somepath", nil)
	if opts.acceptHTML {
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
	} else {
		req.Header.Set("Accept", "*/*")
	}
	req.RemoteAddr = "203.0.113.42:55555"
	ctx := context.WithValue(req.Context(), currentUserKey, viewer)
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()
	f.chain.ServeHTTP(rw, req)
	return rw
}

// anonRequest serves a request as an anonymous IP-bucketed caller.
// Useful shorthand for the loop tests.
func (f *htmlLimitFixture) anonRequest() *httptest.ResponseRecorder {
	return f.serve(CurrentUser{}, htmlBodyOpts{acceptHTML: true})
}

func TestHTMLRateLimit_AnonUnderBurst_Passes(t *testing.T) {
	t.Parallel()
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 3, AnonRefill: 1, AuthedBurst: 100, AuthedRefill: 10,
	})
	for i := 1; i <= 3; i++ {
		rw := f.anonRequest()
		if rw.Code != http.StatusOK {
			t.Fatalf("hit %d: status=%d, want 200", i, rw.Code)
		}
	}
	if f.reached != 3 {
		t.Errorf("terminal reached %d times; want 3", f.reached)
	}
}

func TestHTMLRateLimit_AnonOverBurst_Throttles(t *testing.T) {
	t.Parallel()
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 2, AnonRefill: 1, AuthedBurst: 100, AuthedRefill: 10,
	})
	// Burn the two-hit allowance.
	for i := 0; i < 2; i++ {
		if rw := f.anonRequest(); rw.Code != http.StatusOK {
			t.Fatalf("warmup hit %d: status=%d, want 200", i, rw.Code)
		}
	}
	rw := f.anonRequest()
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd hit status=%d, want 429", rw.Code)
	}
	if got := rw.Header().Get("Retry-After"); got == "" {
		t.Errorf("Retry-After header missing")
	} else if n, _ := strconv.Atoi(got); n < 1 {
		t.Errorf("Retry-After = %q; want positive integer", got)
	}
	if got := rw.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q; want text/html when Accept asks for html", got)
	}
	if !strings.Contains(rw.Body.String(), "Too Many Requests") {
		t.Errorf("body missing 429 marker: %q", rw.Body.String())
	}
}

func TestHTMLRateLimit_NonBrowserGetsPlainText(t *testing.T) {
	t.Parallel()
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 1, AnonRefill: 1, AuthedBurst: 100, AuthedRefill: 10,
	})
	// Burn the single-hit allowance.
	if rw := f.serve(CurrentUser{}, htmlBodyOpts{acceptHTML: false}); rw.Code != http.StatusOK {
		t.Fatalf("warmup: status=%d, want 200", rw.Code)
	}
	rw := f.serve(CurrentUser{}, htmlBodyOpts{acceptHTML: false})
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit non-browser: status=%d, want 429", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q; want text/plain for non-browser client", got)
	}
}

func TestHTMLRateLimit_AuthedUsesHigherBudget(t *testing.T) {
	t.Parallel()
	// Anon burst is 1 so a single hit exhausts it. Authed burst is
	// 5 — same caller authenticated should breeze through.
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 1, AnonRefill: 1, AuthedBurst: 5, AuthedRefill: 5,
	})
	viewer := CurrentUser{ID: 42, Username: "alice"}
	for i := 1; i <= 5; i++ {
		rw := f.serve(viewer, htmlBodyOpts{acceptHTML: true})
		if rw.Code != http.StatusOK {
			t.Fatalf("authed hit %d: status=%d, want 200", i, rw.Code)
		}
	}
	rw := f.serve(viewer, htmlBodyOpts{acceptHTML: true})
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("authed 6th hit: status=%d, want 429", rw.Code)
	}
}

func TestHTMLRateLimit_SiteAdminBypasses(t *testing.T) {
	t.Parallel()
	// Anon burst is intentionally tiny; site admin should never see
	// a 429 even if they fire well above it.
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 1, AnonRefill: 1, AuthedBurst: 1, AuthedRefill: 1,
	})
	viewer := CurrentUser{ID: 1, Username: "root", IsSiteAdmin: true}
	for i := 1; i <= 5; i++ {
		rw := f.serve(viewer, htmlBodyOpts{acceptHTML: true})
		if rw.Code != http.StatusOK {
			t.Fatalf("admin hit %d: status=%d, want 200", i, rw.Code)
		}
	}
	if f.reached != 5 {
		t.Errorf("terminal reached %d; want 5", f.reached)
	}
}

func TestHTMLRateLimit_DistinctIPsAreIndependent(t *testing.T) {
	t.Parallel()
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 1, AnonRefill: 1, AuthedBurst: 100, AuthedRefill: 10,
	})
	// First IP burns its single-hit allowance.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:1111"
	req.Header.Set("Accept", "text/html")
	req = req.WithContext(context.WithValue(req.Context(), currentUserKey, CurrentUser{}))
	rw := httptest.NewRecorder()
	f.chain.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("first IP first hit: status=%d, want 200", rw.Code)
	}
	rw = httptest.NewRecorder()
	f.chain.ServeHTTP(rw, req)
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("first IP second hit: status=%d, want 429", rw.Code)
	}
	// Second IP should still be at full allowance.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "203.0.113.2:2222"
	req2.Header.Set("Accept", "text/html")
	req2 = req2.WithContext(context.WithValue(req2.Context(), currentUserKey, CurrentUser{}))
	rw2 := httptest.NewRecorder()
	f.chain.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusOK {
		t.Fatalf("second IP: status=%d, want 200", rw2.Code)
	}
}

func TestHTMLRateLimit_NilLimiterNoops(t *testing.T) {
	t.Parallel()
	// nil limiter (e.g. tests that don't stand up Postgres) must
	// not 429 anything — the chain is a straight pass-through.
	cfg := HTMLRateLimitConfig{AnonBurst: 1, AnonRefill: 1, AuthedBurst: 1, AuthedRefill: 1}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	chain := HTMLRateLimit(nil, cfg)(terminal)
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.3:3333"
		req = req.WithContext(context.WithValue(req.Context(), currentUserKey, CurrentUser{}))
		rw := httptest.NewRecorder()
		chain.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("nil-limiter hit %d: status=%d, want 200", i, rw.Code)
		}
	}
}

func TestHTMLRateLimit_ZeroBurstFailsOpen(t *testing.T) {
	t.Parallel()
	// Misconfigured budgets (Burst=0) should NOT 429 — they should
	// fall through. Boot-time validation keeps this branch
	// unreachable in practice; the middleware just doesn't
	// pre-judge an operator who fat-fingered the env var.
	f := newHTMLLimitFixture(t, HTMLRateLimitConfig{
		AnonBurst: 0, AnonRefill: 0, AuthedBurst: 0, AuthedRefill: 0,
	})
	rw := f.anonRequest()
	if rw.Code != http.StatusOK {
		t.Fatalf("zero-burst: status=%d, want 200 (fail-open)", rw.Code)
	}
}

func TestBurstToPolicy_TranslatesCorrectly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		burst, refill   int
		wantMax, wantWS int // wantWS = window in seconds
		disabled        bool
	}{
		{60, 1, 60, 60, false},    // sprint default anon
		{600, 10, 600, 60, false}, // sprint default authed
		{5, 5, 5, 1, false},       // burst == refill → 1s window
		{0, 1, 0, 0, true},        // disabled (burst 0)
		{10, 0, 0, 0, true},       // disabled (refill 0)
	}
	for _, c := range cases {
		p := burstToPolicy("test", c.burst, c.refill)
		if c.disabled {
			if p.Max != 0 {
				t.Errorf("burst=%d refill=%d: expected disabled (Max=0), got %+v", c.burst, c.refill, p)
			}
			continue
		}
		if p.Max != c.wantMax {
			t.Errorf("burst=%d refill=%d: Max=%d, want %d", c.burst, c.refill, p.Max, c.wantMax)
		}
		if int(p.Window.Seconds()) != c.wantWS {
			t.Errorf("burst=%d refill=%d: Window=%v, want %ds", c.burst, c.refill, p.Window, c.wantWS)
		}
	}
}
