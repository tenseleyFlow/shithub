// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import "fmt"

// VisibilityPredicate returns a SQL WHERE-clause fragment plus its
// bind args that filters rows from a `repos` table reference (or a
// table that joins to repos via a column named like
// `repo_id`/`r.id`) to those visible to the actor.
//
// The fragment is parameterised against the supplied placeholder
// offset so callers can splice it into queries that already bind
// other parameters. Returns:
//
//	clause — SQL fragment ready to drop after `WHERE` or `AND`.
//	args   — the values for the placeholders inside `clause`, in
//	         order from `$<startPlaceholder>` upward.
//
// `tableAlias` is the alias the caller used for the `repos` table
// (e.g. "r" for `FROM repos r`). The fragment references
// `<tableAlias>.visibility`, `<tableAlias>.owner_user_id`,
// `<tableAlias>.deleted_at`, and `<tableAlias>.id` as needed.
//
// Visibility rules (mirror policy.Can step 4–6):
//
//   - Soft-deleted repos are always excluded.
//   - Public repos visible to anyone.
//   - Private repos visible only to: owner, or any user with a
//     row in `repo_collaborators` (any role).
//
// Site-admin special-cased: when actor.IsSiteAdmin, only the
// soft-delete filter applies (admins can read everything).
//
// This is the single source of truth for "what repos can this
// viewer see in a list query". S28 search composes it; future
// listing endpoints (trending, activity feed) reuse it.
func VisibilityPredicate(actor Actor, tableAlias string, startPlaceholder int) (clause string, args []any) {
	if tableAlias == "" {
		tableAlias = "r"
	}

	// Always exclude soft-deleted.
	base := fmt.Sprintf("%s.deleted_at IS NULL", tableAlias)

	if actor.IsSiteAdmin {
		// Admins see everything that isn't soft-deleted.
		return base, nil
	}

	if actor.IsAnonymous {
		// Public only.
		return fmt.Sprintf(
			"%s AND %s.visibility = 'public'",
			base, tableAlias,
		), nil
	}

	// Logged-in: public OR (owner) OR (collab row exists).
	// Two placeholders consumed: actor.UserID twice (owner check +
	// collab subquery).
	p1 := startPlaceholder
	p2 := startPlaceholder + 1
	clause = fmt.Sprintf(
		"%s AND ("+
			"%s.visibility = 'public' "+
			"OR %s.owner_user_id = $%d "+
			"OR EXISTS (SELECT 1 FROM repo_collaborators c "+
			"WHERE c.repo_id = %s.id AND c.user_id = $%d)"+
			")",
		base, tableAlias, tableAlias, p1, tableAlias, p2,
	)
	args = []any{actor.UserID, actor.UserID}
	return clause, args
}
