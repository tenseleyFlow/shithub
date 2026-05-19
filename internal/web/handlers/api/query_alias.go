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
