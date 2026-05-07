// SPDX-License-Identifier: AGPL-3.0-or-later

package throttle

import (
	"context"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestLimiter_HitAndThrottle(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	l := NewLimiter()

	p := Limit{Scope: "login", Identifier: "ip:1.2.3.4|alice", Max: 3, Window: time.Hour}

	for i := 1; i <= 3; i++ {
		if err := l.Hit(ctx, pool, p); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
	err := l.Hit(ctx, pool, p)
	if !IsThrottled(err) {
		t.Fatalf("4th hit: expected throttled, got %v", err)
	}
}

func TestLimiter_Reset(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	l := NewLimiter()

	p := Limit{Scope: "login", Identifier: "ip:1.2.3.4|bob", Max: 1, Window: time.Hour}

	if err := l.Hit(ctx, pool, p); err != nil {
		t.Fatalf("first hit: %v", err)
	}
	if err := l.Hit(ctx, pool, p); !IsThrottled(err) {
		t.Fatalf("second hit before reset: expected throttled, got %v", err)
	}
	if err := l.Reset(ctx, pool, p.Scope, p.Identifier); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := l.Hit(ctx, pool, p); err != nil {
		t.Fatalf("hit after reset: %v", err)
	}
}

func TestLimiter_WindowReset(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	l := NewLimiter()

	// Window is short enough that the second hit lands in a brand-new
	// window. The bump query resets the counter when the existing window
	// started before (now - Window). Use a generous sleep so clock
	// granularity / connection latency between the Go cutoff and the PG
	// now() can't make the comparison ambiguous.
	p := Limit{Scope: "login", Identifier: "ip:1.2.3.4|carol", Max: 1, Window: 200 * time.Millisecond}

	if err := l.Hit(ctx, pool, p); err != nil {
		t.Fatalf("first hit: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := l.Hit(ctx, pool, p); err != nil {
		t.Fatalf("hit after window: expected fresh window, got %v", err)
	}
}
