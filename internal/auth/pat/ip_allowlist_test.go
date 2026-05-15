// SPDX-License-Identifier: AGPL-3.0-or-later

package pat_test

import (
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
		got, invalid := pat.ParseAllowlist(tc.in)
		if len(invalid) != 0 {
			t.Errorf("ParseAllowlist(%q) unexpected invalid: %v", tc.in, invalid)
		}
		if !stringSliceEqual(got, tc.want) {
			t.Errorf("ParseAllowlist(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseAllowlist_RejectsMalformed(t *testing.T) {
	t.Parallel()
	got, invalid := pat.ParseAllowlist("203.0.113.0/99\nnot-an-ip\n198.51.100.5")
	if len(got) != 1 || got[0] != "198.51.100.5/32" {
		t.Errorf("valid entry should still be parsed: got=%v", got)
	}
	if len(invalid) != 2 {
		t.Errorf("expected 2 invalid entries, got %d (%v)", len(invalid), invalid)
	}
}

func TestParseAllowlist_CapsAtMax(t *testing.T) {
	t.Parallel()
	// Build a list with MaxIPAllowlistEntries+10 unique entries.
	var b strings.Builder
	for i := 0; i < pat.MaxIPAllowlistEntries+10; i++ {
		// Build a /32 for each unique IP.
		oct := i & 0xff
		b.WriteString("203.0.113.")
		b.WriteByte(byte('0' + oct/100))
		b.WriteByte(byte('0' + (oct/10)%10))
		b.WriteByte(byte('0' + oct%10))
		b.WriteString("\n")
	}
	got, _ := pat.ParseAllowlist(b.String())
	if len(got) > pat.MaxIPAllowlistEntries {
		t.Errorf("entries exceeded cap: got %d, max %d", len(got), pat.MaxIPAllowlistEntries)
	}
}

func TestParseAllowlist_RejectsOversizedEntry(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("a", pat.MaxIPAllowlistEntryBytes+1)
	got, invalid := pat.ParseAllowlist(huge)
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
