// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

import "strings"

// PRO-EXT01-11c: route_prefix derivation for usage analytics.
//
// The schema CHECK constraint caps route_prefix at 64 bytes. We
// derive it from the request path by taking at most the first three
// segments, lowercased, with any individual numeric or hash-shaped
// segment replaced by ":id" to keep cardinality bounded. The goal is
// "/api/v1/repos" not "/api/v1/repos/alice/demo" — users want to see
// shapes of usage, not full URL strings.
//
// The CLI and ops dashboards both consume these strings, so the
// derivation is exported and tested.

// maxRoutePrefixSegments caps the segment count after canonicalization.
// Three is enough to disambiguate /api/v1/repos vs /api/v1/orgs vs
// /api/v1/user/keys but doesn't drill into individual IDs.
const maxRoutePrefixSegments = 3

// maxRoutePrefixBytes mirrors the schema check. If a derived prefix
// would exceed the cap, we truncate at the boundary and emit a "…"
// marker so the analytics page reader knows the truth was longer.
const maxRoutePrefixBytes = 64

// RoutePrefix returns the canonicalized prefix for the given URL path.
// Always returns a non-empty string suitable for the schema's NOT NULL
// route_prefix column.
func RoutePrefix(urlPath string) string {
	if urlPath == "" || urlPath == "/" {
		return "/"
	}
	trimmed := strings.Trim(urlPath, "/")
	if trimmed == "" {
		return "/"
	}
	parts := strings.SplitN(trimmed, "/", maxRoutePrefixSegments+1)
	if len(parts) > maxRoutePrefixSegments {
		parts = parts[:maxRoutePrefixSegments]
	}
	// Lower-case + replace per-resource identifiers (numeric or sha-ish)
	// with ":id" so /api/v1/repos/alice/demo and /api/v1/repos/bob/proj
	// collapse to the same bucket /api/v1/repos.
	//
	// Heuristic: any segment that is *purely digits* or is at least 20
	// chars of hex is treated as an identifier. Usernames and repo
	// names slip through, which is fine — we only call this on the
	// first 3 segments where the API surface is route-prefix-shaped.
	for i, p := range parts {
		parts[i] = strings.ToLower(canonicalSegment(p))
	}
	out := "/" + strings.Join(parts, "/")
	if len(out) > maxRoutePrefixBytes {
		// Hard cap. "…" is 3 bytes in UTF-8 — budget for that so the
		// final string fits the schema CHECK constraint exactly. We
		// also walk back from the truncation point if we'd land
		// inside a multi-byte rune.
		cutoff := maxRoutePrefixBytes - len("…")
		for cutoff > 0 && (out[cutoff]&0xC0) == 0x80 {
			cutoff--
		}
		out = out[:cutoff] + "…"
	}
	return out
}

func canonicalSegment(s string) string {
	if s == "" {
		return ""
	}
	if isAllDigits(s) {
		return ":id"
	}
	if len(s) >= 20 && isHex(s) {
		return ":id"
	}
	return s
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
