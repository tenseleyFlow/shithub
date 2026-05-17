// SPDX-License-Identifier: AGPL-3.0-or-later

package pat_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

// TestParseAllowlist_AcceptsCIDRAndBareAddresses pins the parsing
// contract: both forms work and bare addresses are canonicalized to
// /32 (v4) or /128 (v6).
func TestParseAllowlist_AcceptsCIDRAndBareAddresses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"203.0.113.0/24", []string{"203.0.113.0/24"}},
		{"203.0.113.5", []string{"203.0.113.5/32"}},
		{"2001:db8::/32", []string{"2001:db8::/32"}},
		{"::1", []string{"::1/128"}},
		// Newline-separated.
		{"203.0.113.0/24\n198.51.100.5", []string{"203.0.113.0/24", "198.51.100.5/32"}},
		// Comma-separated, with whitespace.
		{"203.0.113.0/24, 198.51.100.5 ", []string{"203.0.113.0/24", "198.51.100.5/32"}},
		// Duplicates are collapsed.
		{"203.0.113.5\n203.0.113.5", []string{"203.0.113.5/32"}},
		// Empty input → nil.
		{"", nil},
		{"   \n   ", nil},
	}
	for _, tc := range cases {
		got, invalid, tooMany := pat.ParseAllowlist(tc.in)
		if len(invalid) != 0 {
			t.Errorf("ParseAllowlist(%q) unexpected invalid: %v", tc.in, invalid)
		}
		if tooMany {
			t.Errorf("ParseAllowlist(%q) unexpected tooMany=true", tc.in)
		}
		if !stringSliceEqual(got, tc.want) {
			t.Errorf("ParseAllowlist(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseAllowlist_RejectsMalformed(t *testing.T) {
	t.Parallel()
	got, invalid, tooMany := pat.ParseAllowlist("203.0.113.0/99\nnot-an-ip\n198.51.100.5")
	if len(got) != 1 || got[0] != "198.51.100.5/32" {
		t.Errorf("valid entry should still be parsed: got=%v", got)
	}
	if len(invalid) != 2 {
		t.Errorf("expected 2 invalid entries, got %d (%v)", len(invalid), invalid)
	}
	if tooMany {
		t.Errorf("did not expect tooMany for this input")
	}
}

// TestParseAllowlist_TooManyEntriesFlagged pins the PRO-EXT_SR2-13
// fix: pre-change the cap was enforced via silent `break`, so a user
// submitting more than MaxIPAllowlistEntries entries had the surplus
// dropped without any signal. The third return value now lets the
// caller surface an explicit error.
//
// Address generation uses /24s starting at 10.0.0.0/24 because
// netip.ParseAddr rejects leading zeros (so the pre-existing test's
// "203.0.113.000"-style entries were all silently invalid, masking
// both the cap behavior and this Q8 regression for the lifetime of
// the test).
func TestParseAllowlist_TooManyEntriesFlagged(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 0; i < pat.MaxIPAllowlistEntries+10; i++ {
		// 10.<i/256>.<i%256>.0/24 — unique for every i up to 65535.
		fmt.Fprintf(&b, "10.%d.%d.0/24\n", i/256, i%256)
	}
	got, invalid, tooMany := pat.ParseAllowlist(b.String())
	if len(invalid) != 0 {
		t.Fatalf("test data should be all-valid: invalid=%v", invalid)
	}
	if len(got) != pat.MaxIPAllowlistEntries {
		t.Errorf("expected exactly %d entries kept, got %d", pat.MaxIPAllowlistEntries, len(got))
	}
	if !tooMany {
		t.Errorf("expected tooMany=true for input with %d entries", pat.MaxIPAllowlistEntries+10)
	}
}

func TestParseAllowlist_RejectsOversizedEntry(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("a", pat.MaxIPAllowlistEntryBytes+1)
	got, invalid, _ := pat.ParseAllowlist(huge)
	if len(got) != 0 || len(invalid) != 1 {
		t.Errorf("oversized entry should be rejected: got=%v invalid=%v", got, invalid)
	}
}

// TestIPMatch_EmptyAllowlistMatchesAll pins the "no restriction"
// invariant — the single most important contract.
func TestIPMatch_EmptyAllowlistMatchesAll(t *testing.T) {
	t.Parallel()
	if !pat.IPMatch(nil, mustAddr("203.0.113.5")) {
		t.Errorf("nil allowlist should match any IP")
	}
	if !pat.IPMatch([]string{}, mustAddr("2001:db8::1")) {
		t.Errorf("empty allowlist should match any IP")
	}
}

// TestIPMatch_CIDRMatchesContainedAddress pins the positive case.
func TestIPMatch_CIDRMatchesContainedAddress(t *testing.T) {
	t.Parallel()
	allow := []string{"203.0.113.0/24"}
	if !pat.IPMatch(allow, mustAddr("203.0.113.5")) {
		t.Errorf("203.0.113.5 should match 203.0.113.0/24")
	}
	if pat.IPMatch(allow, mustAddr("203.0.113.255")) != true {
		t.Errorf("203.0.113.255 should match 203.0.113.0/24")
	}
}

// TestIPMatch_NonContainedAddressDenied pins the negative case.
func TestIPMatch_NonContainedAddressDenied(t *testing.T) {
	t.Parallel()
	allow := []string{"203.0.113.0/24"}
	if pat.IPMatch(allow, mustAddr("198.51.100.5")) {
		t.Errorf("198.51.100.5 should NOT match 203.0.113.0/24")
	}
}

// TestIPMatch_IPv4MappedIPv6Normalized confirms a v4-in-v6 request
// matches v4 prefixes.
func TestIPMatch_IPv4MappedIPv6Normalized(t *testing.T) {
	t.Parallel()
	allow := []string{"203.0.113.0/24"}
	addr := mustAddr("::ffff:203.0.113.5")
	if !pat.IPMatch(allow, addr) {
		t.Errorf("::ffff:203.0.113.5 should match 203.0.113.0/24 after unmapping")
	}
}

// TestIPMatch_MalformedEntryFailsClosed is the security-critical case:
// if any stored CIDR is unparseable, the whole match returns false.
// This protects against a botched migration silently widening access.
func TestIPMatch_MalformedEntryFailsClosed(t *testing.T) {
	t.Parallel()
	allow := []string{"not-a-cidr"}
	if pat.IPMatch(allow, mustAddr("203.0.113.5")) {
		t.Errorf("malformed entry must fail-closed")
	}
}

// TestIPMatch_InvalidAddrAlwaysDenied — same invariant, request side.
func TestIPMatch_InvalidAddrAlwaysDenied(t *testing.T) {
	t.Parallel()
	allow := []string{"203.0.113.0/24"}
	if pat.IPMatch(allow, netip.Addr{}) {
		t.Errorf("zero-value addr must be denied")
	}
}

func mustAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
