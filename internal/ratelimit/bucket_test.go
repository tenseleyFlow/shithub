// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestAllow_UnderLimitThenBlocked(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()
	p := Policy{Scope: "test:underthen", Max: 3, Window: time.Hour}
	key := "k1"

	for i := 1; i <= 3; i++ {
		d, err := l.Allow(ctx, p, key)
		if err != nil {
			t.Fatalf("hit %d err: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("hit %d: expected Allowed; got %v", i, d)
		}
		if d.Limit != 3 {
			t.Errorf("hit %d Limit = %d; want 3", i, d.Limit)
		}
		if d.Remaining != 3-i {
			t.Errorf("hit %d Remaining = %d; want %d", i, d.Remaining, 3-i)
		}
	}
	d, err := l.Allow(ctx, p, key)
	if err != nil {
		t.Fatalf("over-limit err: %v", err)
	}
	if d.Allowed {
		t.Fatalf("4th hit allowed; want blocked")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v; want > 0", d.RetryAfter)
	}
}

func TestAllow_DistinctKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()
	p := Policy{Scope: "test:distinct", Max: 1, Window: time.Hour}

	if d, _ := l.Allow(ctx, p, "alice"); !d.Allowed {
		t.Fatalf("alice first hit blocked: %v", d)
	}
	if d, _ := l.Allow(ctx, p, "bob"); !d.Allowed {
		t.Fatalf("bob first hit blocked: %v", d)
	}
	if d, _ := l.Allow(ctx, p, "alice"); d.Allowed {
		t.Fatalf("alice second hit allowed; want blocked")
	}
}

func TestAcquireLease_BlocksUntilRelease(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()
	p := Policy{Scope: "test:lease", Max: 2, Window: time.Hour}

	lease1, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease1 err: %v", err)
	}
	if !d.Allowed || d.Remaining != 1 {
		t.Fatalf("lease1 decision=%+v", d)
	}
	lease2, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease2 err: %v", err)
	}
	if !d.Allowed || d.Remaining != 0 {
		t.Fatalf("lease2 decision=%+v", d)
	}
	lease3, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease3 err: %v", err)
	}
	if lease3 != nil || d.Allowed {
		t.Fatalf("lease3=(%v,%+v), want blocked without lease", lease3, d)
	}
	if err := lease1.Release(ctx); err != nil {
		t.Fatalf("release lease1: %v", err)
	}
	lease4, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease4 err: %v", err)
	}
	if lease4 == nil || !d.Allowed {
		t.Fatalf("lease4=(%v,%+v), want allowed", lease4, d)
	}
	if err := lease1.Release(ctx); err != nil {
		t.Fatalf("second release lease1: %v", err)
	}
	_ = lease2.Release(ctx)
	_ = lease4.Release(ctx)
}

func TestAcquireLease_RollsStaleWindow(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()
	p := Policy{Scope: "test:lease:ttl", Max: 1, Window: time.Millisecond}

	lease1, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease1 err: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("lease1 blocked: %+v", d)
	}
	time.Sleep(5 * time.Millisecond)
	lease2, d, err := l.AcquireLease(ctx, p, "alice")
	if err != nil {
		t.Fatalf("lease2 err: %v", err)
	}
	if lease2 == nil || !d.Allowed {
		t.Fatalf("stale lease did not roll forward: lease=%v decision=%+v", lease2, d)
	}
	_ = lease1.Release(ctx)
	_ = lease2.Release(ctx)
}

func TestAllow_RejectsBadPolicy(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()
	cases := []Policy{
		{Scope: "", Max: 1, Window: time.Hour},
		{Scope: "x", Max: 0, Window: time.Hour},
		{Scope: "x", Max: 1, Window: 0},
	}
	for _, p := range cases {
		if _, err := l.Allow(ctx, p, "k"); err == nil {
			t.Errorf("Allow(%+v) returned nil err; want non-nil", p)
		}
	}
}

func TestMaskToNetwork(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"192.0.2.5", "192.0.2.0"},
		{"10.0.0.255", "10.0.0.0"},
		{"203.0.113.42", "203.0.113.0"},
	}
	for _, tc := range cases {
		got := maskToNetwork(netip.MustParseAddr(tc.in))
		if got.String() != tc.want {
			t.Errorf("maskToNetwork(%s) = %s; want %s", tc.in, got, tc.want)
		}
	}
	// IPv6 /48
	v6 := maskToNetwork(netip.MustParseAddr("2001:db8:dead:beef::1"))
	if want := "2001:db8:dead::"; v6.String() != want {
		t.Errorf("v6 mask = %s; want %s", v6, want)
	}
}

func TestNetworkKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in, want string
	}{
		{"v4 host bits zeroed", "57.141.2.200", "57.141.2.0/24"},
		{"v4 same network as above", "57.141.2.9", "57.141.2.0/24"},
		{"v4 different network", "57.141.3.9", "57.141.3.0/24"},
		{"v6 masked to /48", "2001:db8:dead:beef::1", "2001:db8:dead::/48"},
		{"v6 same /48", "2001:db8:dead:0:1234::9", "2001:db8:dead::/48"},
		{"ipv4-mapped v6 unwraps to v4", "::ffff:57.141.2.200", "57.141.2.0/24"},
		// Unparseable input keeps the caller's key stable rather
		// than collapsing every bad value into one shared bucket.
		{"garbage passes through", "not-an-ip", "not-an-ip"},
		{"empty passes through", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NetworkKey(tc.in); got != tc.want {
				t.Errorf("NetworkKey(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAllowSignupIP(t *testing.T) {
	t.Parallel()
	l := New(dbtest.NewTestDB(t))
	ctx := context.Background()

	// Two IPs in the same /24 share a counter.
	a := netip.MustParseAddr("198.51.100.5")
	b := netip.MustParseAddr("198.51.100.99")
	d1, _ := l.AllowSignupIP(ctx, a, 2, time.Hour)
	d2, _ := l.AllowSignupIP(ctx, b, 2, time.Hour)
	d3, _ := l.AllowSignupIP(ctx, a, 2, time.Hour)
	if !d1.Allowed || !d2.Allowed {
		t.Fatalf("first two hits should be allowed; got %v %v", d1, d2)
	}
	if d3.Allowed {
		t.Fatalf("third hit (same /24) should be blocked; got %v", d3)
	}

	// A different /24 starts fresh.
	other := netip.MustParseAddr("203.0.113.7")
	d4, _ := l.AllowSignupIP(ctx, other, 2, time.Hour)
	if !d4.Allowed {
		t.Fatalf("different /24 first hit should be allowed; got %v", d4)
	}
}
