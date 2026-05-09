// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

// allowedTargetVisibility enforces the visibility floor: a fork's
// visibility must be ≤ source's. Forking public → private is fine
// (the user is just choosing to keep their copy private); forking
// private → public would expose previously-private content and is
// always rejected.
//
// Returns "" + false when the proposed shape isn't allowed.
func allowedTargetVisibility(source, target string) (string, bool) {
	switch source {
	case "public":
		// Any target visibility is allowed; default to public if blank.
		if target == "" {
			return "public", true
		}
		return target, target == "public" || target == "private"
	case "private":
		// Forking a private repo never expands its reach; target must
		// stay private. Empty defaults to private.
		if target == "" || target == "private" {
			return "private", true
		}
		return "", false
	}
	return "", false
}
