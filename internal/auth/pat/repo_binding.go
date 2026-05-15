// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

// PRO-EXT01-11b: PAT single-repo binding.
//
// SECURITY CONTRACT:
//
//  1. binding == 0 → no restriction. A non-bound token (the default,
//     and the entirety of the Free tier) can act on any repo the
//     scope+visibility rules already allow.
//
//  2. binding != 0 → the token is locked to that single repo. Any
//     downstream handler that resolves a repo for the request MUST
//     call RepoBindingAllows before serving repo-scoped data.
//
//  3. RepoBindingAllows does NOT check scope, visibility, or
//     ownership. Those are the existing controls. This helper only
//     answers the binding question.
//
// The helper is deliberately split out from the middleware so route
// handlers — which know which repo the request is targeting — own the
// enforcement decision. The middleware can't reliably extract a repo
// from the URL path alone (some routes resolve by owner/name, some by
// id, some via run/job/check-run lookups that walk back to a repo).

// RepoBindingAllows reports whether a PAT bound to `binding` can act on
// `requested`. A binding of 0 always allows; otherwise the IDs must match
// exactly. Designed to be called as `if !pat.RepoBindingAllows(...)`
// inside repo-scoped handlers right after the repo is resolved.
func RepoBindingAllows(binding, requested int64) bool {
	if binding == 0 {
		return true
	}
	return binding == requested
}
