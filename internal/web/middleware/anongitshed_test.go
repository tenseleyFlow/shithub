// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const shedTestPath = "/acme/widgets/commits/0123456789abcdef0123456789abcdef01234567"

// shedFixture wraps AnonGitShed around a handler that blocks until
// released, so tests can hold slots open deterministically.
type shedFixture struct {
	chain   http.Handler
	entered chan struct{}
	release chan struct{}
}

func newShedFixture(cfg AnonGitShedConfig) *shedFixture {
	f := &shedFixture{
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
	}
	f.chain = AnonGitShed(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.entered <- struct{}{}
		<-f.release
		w.WriteHeader(http.StatusOK)
	}))
	return f
}

// start runs one request in the background and returns its recorder
// once the request finishes via the returned channel.
func (f *shedFixture) start(req *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		f.chain.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

func (f *shedFixture) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not reached")
	}
}

func anonReq(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func authedReq(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(WithCurrentUserForTest(req.Context(), CurrentUser{ID: 7, Username: "alice"}))
}

func await(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
		return nil
	}
}

func TestAnonGitShed_ShedsWhenSlotsAndQueueAreFull(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1, Queue: 0, QueueWait: time.Second, RetryAfter: 10 * time.Second})

	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)

	rec := httptest.NewRecorder()
	f.chain.ServeHTTP(rec, anonReq(http.MethodGet, shedTestPath))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request: status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "10" {
		t.Errorf("Retry-After = %q, want 10", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for a non-browser client", rec.Header().Get("Content-Type"))
	}

	close(f.release)
	if rec := await(t, first); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", rec.Code)
	}

	// The slot is returned once the first request finishes.
	rec = httptest.NewRecorder()
	f.chain.ServeHTTP(rec, anonReq(http.MethodGet, shedTestPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("request after release: status %d, want 200", rec.Code)
	}
}

func TestAnonGitShed_HTMLBodyForBrowsers(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1})
	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)

	req := anonReq(http.MethodGet, shedTestPath)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	f.chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want text/html", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `href="/login"`) {
		t.Errorf("body should point signed-out visitors at /login")
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want the 1s floor when unset", got)
	}
	close(f.release)
	await(t, first)
}

func TestAnonGitShed_QueuedRequestRunsWhenSlotFrees(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1, Queue: 1, QueueWait: 5 * time.Second})

	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)
	second := f.start(anonReq(http.MethodGet, shedTestPath))

	// The second request is parked: it must not reach the handler
	// while the first still holds the only slot.
	select {
	case <-f.entered:
		t.Fatal("queued request ran while the slot was held")
	case <-time.After(50 * time.Millisecond):
	}

	close(f.release)
	if rec := await(t, first); rec.Code != http.StatusOK {
		t.Fatalf("first: status %d, want 200", rec.Code)
	}
	if rec := await(t, second); rec.Code != http.StatusOK {
		t.Fatalf("queued: status %d, want 200", rec.Code)
	}
}

func TestAnonGitShed_QueueWaitExpires(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1, Queue: 1, QueueWait: 20 * time.Millisecond})
	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)

	rec := await(t, f.start(anonReq(http.MethodGet, shedTestPath)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 after the queue wait expires", rec.Code)
	}
	close(f.release)
	await(t, first)
}

func TestAnonGitShed_QueueDepthIsBounded(t *testing.T) {
	t.Parallel()
	const queue, extra = 2, 10
	f := newShedFixture(AnonGitShedConfig{Slots: 1, Queue: queue, QueueWait: 5 * time.Second})
	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)

	// Fire well past the queue depth. Exactly `queue` of them may
	// wait; the rest are shed at once, without waiting out QueueWait.
	results := make(chan *httptest.ResponseRecorder, extra)
	for range extra {
		go func() {
			rec := httptest.NewRecorder()
			f.chain.ServeHTTP(rec, anonReq(http.MethodGet, shedTestPath))
			results <- rec
		}()
	}
	for i := range extra - queue {
		select {
		case rec := <-results:
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("overflow request %d: status %d, want 503", i, rec.Code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d overflow requests were shed promptly", i, extra-queue)
		}
	}
	select {
	case rec := <-results:
		t.Fatalf("a queued request finished early with status %d", rec.Code)
	case <-time.After(50 * time.Millisecond):
	}

	// Releasing the handler lets the holder and both waiters through.
	close(f.release)
	if rec := await(t, first); rec.Code != http.StatusOK {
		t.Fatalf("holder: status %d, want 200", rec.Code)
	}
	for range queue {
		select {
		case rec := <-results:
			if rec.Code != http.StatusOK {
				t.Fatalf("queued request: status %d, want 200", rec.Code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("queued request did not finish")
		}
	}
}

func TestAnonGitShed_CanceledWhileQueuedIsShed(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1, Queue: 1, QueueWait: 5 * time.Second})
	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)

	ctx, cancel := context.WithCancel(context.Background())
	second := f.start(anonReq(http.MethodGet, shedTestPath).WithContext(ctx))
	time.Sleep(20 * time.Millisecond)
	cancel()
	if rec := await(t, second); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 when the client goes away in the queue", rec.Code)
	}
	close(f.release)
	await(t, first)
}

// TestAnonGitShed_Bypasses pins who is never gated: signed-in users,
// non-git routes, and writes. Each must reach the handler while the
// only slot is held by an anonymous git read.
func TestAnonGitShed_Bypasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  func() *http.Request
	}{
		{"signed-in user", func() *http.Request { return authedReq(shedTestPath) }},
		{"explore", func() *http.Request { return anonReq(http.MethodGet, "/explore") }},
		{"repo home", func() *http.Request { return anonReq(http.MethodGet, "/acme/widgets") }},
		{"issues", func() *http.Request { return anonReq(http.MethodGet, "/acme/widgets/issues/4") }},
		{"post", func() *http.Request { return anonReq(http.MethodPost, shedTestPath) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newShedFixture(AnonGitShedConfig{Slots: 1})
			holder := f.start(anonReq(http.MethodGet, shedTestPath))
			f.awaitEntered(t)

			bypass := f.start(tc.req())
			f.awaitEntered(t)

			close(f.release)
			if rec := await(t, bypass); rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			await(t, holder)
		})
	}
}

func TestAnonGitShed_DisabledIsPassThrough(t *testing.T) {
	t.Parallel()
	for _, slots := range []int{0, -1} {
		f := newShedFixture(AnonGitShedConfig{Slots: slots})
		close(f.release)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				f.chain.ServeHTTP(rec, anonReq(http.MethodGet, shedTestPath))
				if rec.Code != http.StatusOK {
					t.Errorf("slots=%d: status %d, want 200", slots, rec.Code)
				}
			}()
		}
		wg.Wait()
	}
}

func TestAnonGitShed_HeadIsGated(t *testing.T) {
	t.Parallel()
	f := newShedFixture(AnonGitShedConfig{Slots: 1})
	first := f.start(anonReq(http.MethodGet, shedTestPath))
	f.awaitEntered(t)
	rec := httptest.NewRecorder()
	f.chain.ServeHTTP(rec, anonReq(http.MethodHead, shedTestPath))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HEAD: status %d, want 503", rec.Code)
	}
	close(f.release)
	await(t, first)
}

func TestIsGitBackedReadPath(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/acme/widgets/commits/trunk":           true,
		"/acme/widgets/commits/trunk.atom":      true,
		"/acme/widgets/commit/abc123":           true,
		"/acme/widgets/blob/trunk/README.md":    true,
		"/acme/widgets/raw/trunk/README.md":     true,
		"/acme/widgets/tree/feature/x/internal": true,
		"/acme/widgets/blame/trunk/main.go":     true,
		"/acme/widgets/compare/a...b":           true,
		"/acme/widgets/find/trunk":              true,
		"/":                                     false,
		"/explore":                              false,
		"/acme":                                 false,
		"/acme/widgets":                         false,
		"/acme/widgets/":                        false,
		"/acme/widgets/commits":                 false,
		"/acme/widgets/commits/":                false,
		"/acme/widgets/issues/1":                false,
		"/acme/widgets/pulls":                   false,
		"/acme/widgets.git/info/refs":           false,
		"//widgets/blob/trunk/x":                false,
	}
	for path, want := range cases {
		if got := isGitBackedReadPath(path); got != want {
			t.Errorf("isGitBackedReadPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDefaultAnonGitShedConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultAnonGitShedConfig(3)
	if cfg.Slots != 3 || cfg.Queue != 12 || cfg.QueueWait != 2*time.Second || cfg.RetryAfter != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}
