// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

// Scope is a typed scope constant. The set is intentionally coarse —
// matches the GitHub classic-PAT model. Fine-grained per-repo scoping
// is a post-MVP project.
type Scope string

const (
	ScopeRepoRead  Scope = "repo:read"
	ScopeRepoWrite Scope = "repo:write"
	ScopeUserRead  Scope = "user:read"
	ScopeUserWrite Scope = "user:write"
	ScopeAdminRead Scope = "admin:read"
)

// AllScopes is the canonical ordered list — drives the create-form UI
// and the validation list in ValidScope.
var AllScopes = []Scope{
	ScopeRepoRead,
	ScopeRepoWrite,
	ScopeUserRead,
	ScopeUserWrite,
	ScopeAdminRead,
}

// ValidScope reports whether s is a known scope name.
func ValidScope(s string) bool {
	for _, sc := range AllScopes {
		if string(sc) == s {
			return true
		}
	}
	return false
}

// HasScope reports whether held grants required. Operations that don't
// require a scope (browser-session callers) skip this check entirely.
//
// repo:write implicitly grants repo:read; user:write implicitly grants
// user:read. We expand the implication here so callers don't have to.
func HasScope(held []string, required Scope) bool {
	want := string(required)
	for _, h := range held {
		if h == want {
			return true
		}
		if required == ScopeRepoRead && h == string(ScopeRepoWrite) {
			return true
		}
		if required == ScopeUserRead && h == string(ScopeUserWrite) {
			return true
		}
	}
	return false
}

// NormalizeScopes filters input to known scopes, deduplicates, and
// returns them in AllScopes order. Used by the create handler to
// canonicalize whatever the form submitted before it lands in the DB.
func NormalizeScopes(in []string) []string {
	seen := map[string]struct{}{}
	for _, s := range in {
		if ValidScope(s) {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for _, sc := range AllScopes {
		if _, ok := seen[string(sc)]; ok {
			out = append(out, string(sc))
		}
	}
	return out
}
