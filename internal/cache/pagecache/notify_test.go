// SPDX-License-Identifier: AGPL-3.0-or-later

package pagecache_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cache/pagecache"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// drainOne starts the Listen loop, sends one Publish, and returns
// whichever (repoID, oid) the listener observed. Times out the
// test if the round trip exceeds 3 seconds — much more than a
// healthy local Postgres needs (<100ms is typical).
func drainOne(t *testing.T, repoID int64, oid string) (gotRepo int64, gotOID string) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu    sync.Mutex
		fired = make(chan struct{}, 1)
	)
	apply := func(rid int64, o string) {
		mu.Lock()
		gotRepo = rid
		gotOID = o
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	}

	go pagecache.Listen(ctx, pool, apply, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// LISTEN setup is racy with Publish — give the listener a beat
	// to issue its LISTEN statement before publishing.
	time.Sleep(150 * time.Millisecond)

	if err := pagecache.Publish(ctx, pool, repoID, oid); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for listener to receive notification")
	}
	return gotRepo, gotOID
}

func TestPublishListen_RoundTrip(t *testing.T) {
	t.Parallel()
	gotRepo, gotOID := drainOne(t, 42, "deadbeefcafefacefeedfacedeadbeef00112233")
	if gotRepo != 42 {
		t.Errorf("repo_id round-trip: got %d, want 42", gotRepo)
	}
	if gotOID != "deadbeefcafefacefeedfacedeadbeef00112233" {
		t.Errorf("branch_oid round-trip: got %q, want the fixture", gotOID)
	}
}

func TestPublish_RejectsEmptyOID(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	if err := pagecache.Publish(context.Background(), pool, 1, ""); err == nil {
		t.Errorf("Publish with empty branchOID should error")
	}
}

func TestPublish_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if err := pagecache.Publish(context.Background(), nil, 1, "abc"); err == nil {
		t.Errorf("Publish with nil db should error")
	}
}

func TestListen_NilArgsDoNotPanic(t *testing.T) {
	t.Parallel()
	// Defensive: a misconfigured boot (nil pool or nil apply) must
	// be a clean no-op, not a goroutine that panics late and
	// crashes the web process.
	pagecache.Listen(context.Background(), nil, nil, nil)
	pagecache.Listen(context.Background(), nil, func(int64, string) {}, nil)
}

func TestListen_BadPayloadSwallowed(t *testing.T) {
	t.Parallel()
	// A malformed payload (e.g. a third-party tool firing NOTIFY on
	// our channel) must not stop the listener — the next valid
	// payload should still get applied.
	pool := dbtest.NewTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan struct{}, 1)
	apply := func(int64, string) {
		select {
		case fired <- struct{}{}:
		default:
		}
	}
	go pagecache.Listen(ctx, pool, apply, slog.New(slog.NewTextHandler(io.Discard, nil)))
	time.Sleep(150 * time.Millisecond)

	// First: a malformed NOTIFY. The listener must log + continue.
	if _, err := pool.Exec(ctx, "SELECT pg_notify($1, $2)", pagecache.Channel, "not-json"); err != nil {
		t.Fatalf("bad-payload pg_notify: %v", err)
	}
	// Then: a valid one. We expect the listener to handle it.
	if err := pagecache.Publish(ctx, pool, 7, "abc"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("listener stuck after bad payload — must keep processing")
	}
}
