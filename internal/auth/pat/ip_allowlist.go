// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

import (
	"net/netip"
	"strings"
)

// PRO-EXT01-11a: PAT IP allowlist support.
//
// SECURITY CONTRACT (the entry point for code-review pass):
//
//  1. An empty allowlist is treated as "no restriction" — every Free
//     user's token + every Pro user's token that didn't attach an
//     allowlist sees IPMatch return true. We never silently *deny*
//     when the user didn't ask for restriction.
//
//  2. A non-empty allowlist with any malformed entry causes IPMatch
//     to return false. Parse errors are surfaced at WRITE time (the
//     handler refuses to persist a malformed CIDR); the middleware
//     read path treats any unparseable entry as a hard fail-closed
//     so a botched migration can't accidentally widen access.
//
//  3. The IP we test against is the *trusted* client IP — the
//     middleware sources it via chi/middleware/RealIP, which honors
//     only the operator-configured trusted-proxy set. Tests pin this
//     contract by feeding a raw netip.Addr (no header parsing here).

// MaxIPAllowlistEntries caps how many CIDR ranges a single token can
// carry. 64 is generous for any real network deployment and bounds
// the per-request comparison cost.
const MaxIPAllowlistEntries = 64

// MaxIPAllowlistEntryBytes caps the size of one CIDR string. 64 is
// enough for an IPv6 prefix with a port-like suffix; tighter than
// "no limit" to prevent abuse of the column size constraint.
const MaxIPAllowlistEntryBytes = 64

// ParseAllowlist parses a textarea-style input (newline or comma
// separated) into a deduplicated, validated list of CIDR strings.
// Returns (canonicalized, invalid): canonicalized is the parsed-and-
// re-stringified set (suitable to persist); invalid is the list of
// inputs that failed parsing (suitable for an inline error message).
//
// An empty input returns (nil, nil) so the user can clear the
// allowlist by submitting a blank textarea.
func ParseAllowlist(raw string) (canonicalized []string, invalid []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	// Split on newline or comma. Single trim per entry afterwards.
	rawEntries := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ',' || r == '\r'
	})
	seen := make(map[string]struct{}, len(rawEntries))
	out := make([]string, 0, len(rawEntries))
	for _, e := range rawEntries {
		entry := strings.TrimSpace(e)
		if entry == "" {
			continue
		}
		if len(entry) > MaxIPAllowlistEntryBytes {
			invalid = append(invalid, entry)
			continue
		}
		// Accept both bare addresses ("203.0.113.5") and CIDR
		// ("203.0.113.0/24"). Bare addresses are coerced to a /32 or
		// /128 for storage so the runtime check is uniformly a
		// netip.Prefix.Contains.
		prefix, perr := parseAllowlistEntry(entry)
		if perr != nil {
			invalid = append(invalid, entry)
			continue
		}
		canonical := prefix.String()
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
		if len(out) >= MaxIPAllowlistEntries {
			break
		}
	}
	if len(out) == 0 {
		return nil, invalid
	}
	return out, invalid
}

// parseAllowlistEntry returns the canonical netip.Prefix for one
// entry. Bare addresses are normalized to /32 (IPv4) or /128 (IPv6).
func parseAllowlistEntry(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	bits := 32
	if addr.Is6() {
		bits = 128
	}
	return netip.PrefixFrom(addr, bits), nil
}

// IPMatch reports whether `ip` is permitted under `allowlist`. Empty
// allowlist always matches. A malformed entry causes the match to
// fail-closed (return false) so a botched migration doesn't silently
// widen access. The behavior is documented in the file header.
func IPMatch(allowlist []string, ip netip.Addr) bool {
	if len(allowlist) == 0 {
		return true
	}
	if !ip.IsValid() {
		return false
	}
	// Unmap IPv4-in-IPv6 representations so a request that arrives
	// over a v6 stack but represents a v4 IP still matches v4 prefixes.
	ip = ip.Unmap()
	for _, entry := range allowlist {
		prefix, err := parseAllowlistEntry(entry)
		if err != nil {
			// Fail-closed: a malformed stored entry is treated as
			// "user expressed a restriction we can't honor", not as
			// "no restriction".
			return false
		}
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
