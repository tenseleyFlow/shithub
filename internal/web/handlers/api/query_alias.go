// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"strings"
)

// firstQueryParam returns the first non-empty trimmed value from the
// query string, scanning the supplied parameter names in order. Used to
// accept gh-canonical aliases alongside shithub-native names without
// duplicating filter logic.
//
// Example: firstQueryParam(r, "author", "creator") accepts either spelling.
// Empty inputs return "" — callers treat that as "no filter supplied".
func firstQueryParam(r *http.Request, names ...string) string {
	q := r.URL.Query()
	for _, n := range names {
		if v := strings.TrimSpace(q.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// firstQueryParamRaw returns the first non-empty BYTE-EXACT value from
// the query string. H3 (H7): the trimmed variant above silently strips
// whitespace, so `?head=feat1 ` (trailing space) matched the same row
// as `?head=feat1`. For ref/state/enum filters that should be exact,
// use this variant so whitespace-padded values fall through to the
// validation predicate (which 422s).
func firstQueryParamRaw(r *http.Request, names ...string) string {
	q := r.URL.Query()
	for _, n := range names {
		if v, ok := q[n]; ok && len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}
