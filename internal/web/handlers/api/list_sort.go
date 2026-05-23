// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"fmt"
	"net/http"
)

// validateSortDirection enforces the sort+direction allow-list on a
// list endpoint. F2-4: pre-fix the server silently accepted any value
// for `sort`/`direction`/`order` (the last being a gh-canonical alias
// for `direction`) and returned an unfiltered list. Bogus values now
// 422 rather than silently no-op.
//
// `sortVals` is the endpoint-specific allow-list. We currently honor
// only `direction`/`order` for ordering — the actual ORDER BY isn't
// pluggable yet — so validation is the contract: callers asking for
// unsupported sorts learn before consuming the response.
func validateSortDirection(r *http.Request, sortVals []string) error {
	if v := firstQueryParamRaw(r, "sort"); v != "" {
		if !contains(sortVals, v) {
			return fmt.Errorf("sort: must be one of %s (got %q)", join(sortVals), v)
		}
	}
	// `order` is gh's older spelling; `direction` is the modern one.
	// Both must be asc|desc when present.
	for _, name := range []string{"direction", "order"} {
		if v := firstQueryParamRaw(r, name); v != "" {
			if v != "asc" && v != "desc" {
				return fmt.Errorf("%s: must be one of asc, desc (got %q)", name, v)
			}
		}
	}
	return nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
