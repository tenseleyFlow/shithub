// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

// AllowedUsesAliases is the closed allowlist of step `uses:` references
// the v1 parser accepts. Anything else is a parser error.
//
// The decision (campaign §"What we're cribbing" → "implicit checkout"):
// v1 ships with a small set of first-party magic aliases and refuses
// everything else. Community/Docker `uses:` is parked for v2 because it
// requires a marketplace + sandbox-per-action contract we don't have.
// Keeping this allowlist explicit keeps the precedent honest.
//
//	actions/checkout@v4         — shallow-clones the repo into the
//	                              workspace before steps run. The
//	                              runner does this with its own git
//	                              binary (S41d), not via a containerized
//	                              external action.
//
//	shithub/upload-artifact@v1  — calls the runner's artifact-upload
//	                              path which signs an S3 PUT URL via
//	                              shithubd's API.
//
//	shithub/download-artifact@v1 — fetches an artifact uploaded earlier
//	                               in the same run (or, post-S41d, from
//	                               a parent run via parent_run_id).
//
//	actions/setup-python@v5    — first-party compatibility shim for the
//	                             hosted runner image/toolcache. It validates
//	                             python-version and mutates PATH for later
//	                             steps without fetching marketplace code.
//
// Adding another alias requires:
//  1. Reviewer-required note in the commit message explaining what
//     the alias does and why it can't be a `run:` step.
//  2. Coverage in tests/fixtures/workflows/.
//  3. Update to the migration CHECK constraint
//     (workflow_steps_uses_alias_known) AND a corresponding migration.
var AllowedUsesAliases = map[string]struct{}{
	"actions/checkout@v4":          {},
	"actions/setup-python@v5":      {},
	"shithub/upload-artifact@v1":   {},
	"shithub/download-artifact@v1": {},
}

// IsAllowedUses reports whether ref is a recognized `uses:` alias.
func IsAllowedUses(ref string) bool {
	_, ok := AllowedUsesAliases[ref]
	return ok
}
