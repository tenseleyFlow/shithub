// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth contains shared auth primitives used across the auth
// surface (handlers, middleware, services). Reserved-name validation
// lives here because it's read by signup *and* by username-change in S10.
package auth

import "strings"

// reservedNames is the set of usernames that may NOT be claimed by users.
// It covers:
//   - Every static top-level route shithub registers (so /login can never
//     resolve to a user named "login").
//   - GitHub-known reservations we choose to mirror for parity.
//   - A buffer of likely-future routes we'd rather lock in now than fight
//     to reclaim from a squatter later.
//
// MAINTENANCE: when adding a new top-level route in any sprint, add the
// leading path segment here. The audit test in
// internal/web/handlers/handlers_test.go enforces this by walking the
// registered chi router and checking every static segment.
var reservedNames = map[string]struct{}{
	// shithub-registered routes (S02–S05+).
	"static":        {},
	"healthz":       {},
	"readyz":        {},
	"metrics":       {},
	"signup":        {},
	"login":         {},
	"logout":        {},
	"password":      {},
	"verify-email":  {},
	"internal":      {},
	"settings":      {},
	"api":           {},
	"admin":         {},
	"new":           {},
	"explore":       {},
	"trending":      {},
	"marketplace":   {},
	"issues":        {},
	"pulls":         {},
	"notifications": {},
	"watching":      {},
	"stars":         {},
	"orgs":          {},
	"organizations": {},
	"search":        {},
	"about":         {},
	"contact":       {},
	"pricing":       {},
	"site":          {},
	"help":          {},
	"docs":          {},
	"blog":          {},
	"security":      {},
	"status":        {},
	"terms":         {},
	"privacy":       {},
	"cookies":       {},
	"oauth":         {},
	"sessions":      {},
	"saml":          {},
	"sso":           {},
	"webauthn":      {},
	"two-factor":    {},
	"recovery":      {},
	"avatar":        {},
	"avatars":       {},
	"raw":           {},
	"media":         {},
	"assets":        {},
	"favicon.ico":   {},
	"robots.txt":    {},
	"sitemap.xml":   {},
	"git":           {},
	"ssh":           {},
	"public":        {},
	"private":       {},
	"shithub":       {},
	"shithubd":      {},
	"shithubbot":    {},
}

// IsReserved reports whether name is on the reserved list. The check is
// case-insensitive — usernames are stored as citext, so we normalize here
// to match.
func IsReserved(name string) bool {
	_, ok := reservedNames[strings.ToLower(name)]
	return ok
}

// ReservedNames returns a copy of the reserved set as a slice. Used by
// the route-audit test in handlers_test.go to confirm every registered
// top-level path segment is present here.
func ReservedNames() []string {
	out := make([]string, 0, len(reservedNames))
	for n := range reservedNames {
		out = append(out, n)
	}
	return out
}
