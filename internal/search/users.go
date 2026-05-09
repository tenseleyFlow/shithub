// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"fmt"
)

// SearchUsers runs a user search. No visibility predicate — user
// profiles are public by definition; suspended/deleted accounts are
// excluded so they don't taint search results (matches the spec
// pitfall: "search on suspended user content").
func SearchUsers(ctx context.Context, deps Deps, q ParsedQuery, limit, offset int) ([]UserResult, int64, error) {
	if !q.HasContent() {
		return nil, 0, ErrEmptyQuery
	}
	tsText, tsCtor, hasFTS := tsQueryBindAndCtor(q)
	if !hasFTS {
		// Operators don't apply to user search; with no FTS payload
		// there's nothing to match.
		return nil, 0, nil
	}

	args := []any{tsText, limit, offset}
	queryStr := fmt.Sprintf(`
		SELECT u.id, u.username::text, u.display_name, coalesce(u.bio, ''),
		       ts_rank_cd(s.tsv, %[1]s('shithub_search', $1)) AS rank
		FROM users_search s
		JOIN users u ON u.id = s.user_id
		WHERE s.tsv @@ %[1]s('shithub_search', $1)
		  AND u.suspended_at IS NULL
		  AND u.deleted_at IS NULL
		ORDER BY rank DESC, u.username
		LIMIT $2 OFFSET $3
	`, tsCtor)
	rows, err := deps.Pool.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()
	out := make([]UserResult, 0, limit)
	for rows.Next() {
		var r UserResult
		if err := rows.Scan(&r.ID, &r.Username, &r.DisplayName, &r.Bio, &r.Rank); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	countQuery := fmt.Sprintf(`
		SELECT count(*) FROM users_search s
		JOIN users u ON u.id = s.user_id
		WHERE s.tsv @@ %[1]s('shithub_search', $1)
		  AND u.suspended_at IS NULL
		  AND u.deleted_at IS NULL
	`, tsCtor)
	var total int64
	if err := deps.Pool.QueryRow(ctx, countQuery, tsText).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	return out, total, nil
}
