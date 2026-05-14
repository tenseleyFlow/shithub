// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpcache holds the HTTP-caching primitives the F01 sprint
// uses to short-circuit re-rendering of expensive read endpoints
// (/commits/{branch} first; /tree, /blob to follow). The package is
// deliberately tiny — the helpers compose with the existing
// rendering handlers rather than wrapping them.
//
// Two pieces ship in PR-1:
//
//   - ETag derives a stable, deterministic entity tag from
//     (repo_id, branch_oid, page). A push that moves the branch
//     yields a new head OID and therefore a new ETag with no
//     per-revision cache machinery on our side.
//
//   - IfNoneMatch parses the request's If-None-Match header and
//     reports whether our server-side ETag is in the list, honoring
//     RFC 7232 §3.2 weak comparison and the `*` wildcard.
//
// Subsequent PRs wire these into the /commits handler (PR-2), an
// in-process rendered-HTML LRU (PR-3), and push-side invalidation
// of that LRU (PR-4).
package httpcache

import (
	"fmt"
	"net/http"
	"strings"
)

// ETag returns a strong, double-quoted entity tag uniquely
// identifying a (repo, branch-head, page) tuple. The OID component
// is the invalidation lever: a push that moves the branch produces
// a new head OID and therefore a new ETag without any
// per-revision cache machinery.
//
// Format: `"<repo_id>-<oid>-<page>"`. We don't HMAC-sign — ETags
// aren't security tokens; clients only see them in their own
// If-None-Match headers, and the weak-comparison rules below mean
// the worst a forged value can buy is a 304 the client itself
// asked for. The 40-hex OID component already provides enough
// entropy to avoid accidental collisions across repos.
func ETag(repoID int64, branchOID string, page int) string {
	return fmt.Sprintf(`"%d-%s-%d"`, repoID, branchOID, page)
}

// IfNoneMatch reports whether the request's If-None-Match header
// contains the given server-side etag, honoring:
//
//   - RFC 7232 §3.2 weak comparison: a `W/"x"` on either side
//     matches a `"x"` on the other; only the opaque-tag bytes
//     between the quotes need to be byte-identical.
//   - `*` wildcard: per the RFC, `If-None-Match: *` matches any
//     existing representation. For our purposes — pages whose
//     content always exists when the handler reaches this check —
//     `*` always yields true.
//   - Comma-separated lists with arbitrary surrounding whitespace.
//
// Returns false when either header or etag is empty; the caller
// should treat that as "no cache short-circuit" and fall through
// to the normal render path.
func IfNoneMatch(r *http.Request, etag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" || etag == "" {
		return false
	}
	server := opaqueTag(etag)
	for _, token := range strings.Split(header, ",") {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		if t == "*" {
			return true
		}
		if opaqueTag(t) == server {
			return true
		}
	}
	return false
}

// opaqueTag strips the optional weak-validator prefix `W/` so the
// caller can compare the remaining quoted-string by byte equality.
// Whitespace between `W/` and the opening quote is malformed per
// RFC 7232 §2.3 ABNF, but well-behaved clients never emit it; we
// don't try to repair it.
func opaqueTag(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "W/")
}
