// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// SearchRepos runs a repo search visible to actor. limit / offset
// drive paging.
//
// Ranking: `ts_rank_cd * (1 + ln(1 + star_count)) * recency_decay`
// where recency_decay is `1 / (1 + days_since_update / 30)`. The
// whole rank computation lives in SQL so Postgres can short-circuit
// on the GIN index.
func SearchRepos(ctx context.Context, deps Deps, actor policy.Actor, q ParsedQuery, limit, offset int) ([]RepoResult, int64, error) {
	if !q.HasContent() {
		return nil, 0, ErrEmptyQuery
	}

	tsText, tsCtor, hasFTS := tsQueryBindAndCtor(q)

	// repo:owner/name AND no free-text → list that one repo.
	if !hasFTS && q.RepoFilter == nil {
		return nil, 0, nil
	}

	// $1 is the tsquery text payload (only when hasFTS); the
	// visibility predicate gets the next placeholders.
	args := []any{}
	tsPlaceholder := 0
	if hasFTS {
		args = append(args, tsText)
		tsPlaceholder = len(args)
	}
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", len(args)+1)
	args = append(args, visArgs...)

	repoFilter := ""
	if q.RepoFilter != nil {
		ownerPos := len(args) + 1
		namePos := len(args) + 2
		args = append(args, q.RepoFilter.Owner, q.RepoFilter.Name)
		repoFilter = fmt.Sprintf(
			" AND r.id = (SELECT r2.id FROM repos r2 JOIN users u2 ON u2.id = r2.owner_user_id "+
				"WHERE u2.username = $%d AND r2.name = $%d AND r2.deleted_at IS NULL)",
			ownerPos, namePos,
		)
	}

	whereFTS := "TRUE"
	rankExpr := "1.0"
	if hasFTS {
		whereFTS = fmt.Sprintf("rs.tsv @@ %s('shithub_search', $%d)", tsCtor, tsPlaceholder)
		rankExpr = fmt.Sprintf("ts_rank_cd(rs.tsv, %s('shithub_search', $%d))", tsCtor, tsPlaceholder)
	}

	limPos := len(args) + 1
	offPos := len(args) + 2
	args = append(args, limit, offset)

	queryStr := fmt.Sprintf(`
		SELECT r.id, u.username, r.name, r.description, r.visibility::text,
		       r.star_count, r.updated_at,
		       %[1]s
		           * (1.0 + ln(1.0 + r.star_count))
		           * (1.0 / (1.0 + EXTRACT(EPOCH FROM (now() - r.updated_at)) / 86400.0 / 30.0))
		       AS rank
		FROM repos_search rs
		JOIN repos r  ON r.id = rs.repo_id
		JOIN users u  ON u.id = r.owner_user_id
		WHERE %[2]s
		  AND %[3]s
		  %[4]s
		ORDER BY rank DESC, r.updated_at DESC
		LIMIT $%[5]d OFFSET $%[6]d
	`, rankExpr, whereFTS, visClause, repoFilter, limPos, offPos)

	rows, err := deps.Pool.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search repos: %w", err)
	}
	defer rows.Close()
	out := make([]RepoResult, 0, limit)
	for rows.Next() {
		var r RepoResult
		if err := rows.Scan(&r.ID, &r.OwnerUsername, &r.Name, &r.Description,
			&r.Visibility, &r.StarCount, &r.UpdatedAt, &r.Rank); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Total count for pagination — re-runs the WHERE without the
	// LIMIT/OFFSET tail.
	countQuery := fmt.Sprintf(`
		SELECT count(*)
		FROM repos_search rs
		JOIN repos r  ON r.id = rs.repo_id
		JOIN users u  ON u.id = r.owner_user_id
		WHERE %[1]s AND %[2]s %[3]s
	`, whereFTS, visClause, repoFilter)
	var total int64
	if err := deps.Pool.QueryRow(ctx, countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count repos: %w", err)
	}
	return out, total, nil
}

// tsQueryBindAndCtor returns the tsquery payload + the SQL
// constructor function name, plus a flag indicating whether there's
// any FTS payload to bind. Phrase wins over free text when supplied.
//
// The SQL constructor is one of:
//
//	plainto_tsquery('shithub_search', $N)
//	phraseto_tsquery('shithub_search', $N)
//
// Both are user-input safe — they accept arbitrary text without
// rejecting malformed boolean syntax (unlike `to_tsquery`).
func tsQueryBindAndCtor(q ParsedQuery) (text, ctor string, hasFTS bool) {
	if q.Phrase != "" {
		return q.Phrase, "phraseto_tsquery", true
	}
	if q.Text != "" {
		return q.Text, "plainto_tsquery", true
	}
	return "", "", false
}
