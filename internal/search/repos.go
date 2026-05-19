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

	// At least one signal must drive the query: free-text, a
	// repo:owner/name pair, an owner filter, or a structured repo
	// qualifier such as language:/created:/updated:.
	if !hasFTS && q.RepoFilter == nil && q.OwnerFilter == "" &&
		q.LanguageFilter == "" && q.CreatedFilter == nil && q.UpdatedFilter == nil {
		return nil, 0, nil
	}

	// G11 (F49): if free-text was supplied without any narrowing
	// filter (repo:/user:/org:), verify the user gave at least one
	// token long enough to plausibly match indexed content. Short
	// queries like "F" are syntactically valid but never produce hits,
	// so we surface a typed 422 instead of letting the CLI report
	// "no results found" the same as a real no-match search.
	if hasFTS && q.RepoFilter == nil && q.OwnerFilter == "" {
		if err := validateFTSNotShortOnly(tsText); err != nil {
			return nil, 0, err
		}
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

	extraWhere := ""
	if q.RepoFilter != nil {
		ownerPos := len(args) + 1
		namePos := len(args) + 2
		args = append(args, q.RepoFilter.Owner, q.RepoFilter.Name)
		extraWhere += repoFilterByOwnerName("r", ownerPos, namePos)
	}
	if q.OwnerFilter != "" {
		// E-audit E23: `user:foo` / `org:foo` match the owning user
		// OR org slug. gh aliases them; we mirror that here.
		ownerPos := len(args) + 1
		args = append(args, q.OwnerFilter)
		extraWhere += fmt.Sprintf(
			" AND (u.username = $%d OR o.slug = $%d)",
			ownerPos, ownerPos,
		)
	}
	extraWhere += appendCITextFilter(&args, "coalesce(r.primary_language, '')", q.LanguageFilter)
	extraWhere += appendDateRangeFilter(&args, "r.created_at", q.CreatedFilter)
	extraWhere += appendDateRangeFilter(&args, "r.updated_at", q.UpdatedFilter)

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
		SELECT r.id, %[7]s, r.name, r.description, r.visibility::text,
		       r.star_count, r.updated_at,
		       %[1]s
		           * (1.0 + ln(1.0 + r.star_count))
		           * (1.0 / (1.0 + EXTRACT(EPOCH FROM (now() - r.updated_at)) / 86400.0 / 30.0))
		       AS rank
		FROM repos_search rs
		JOIN repos r  ON r.id = rs.repo_id
		%[8]s
		WHERE %[2]s
		  AND %[3]s
		  %[4]s
		ORDER BY rank DESC, r.updated_at DESC
		LIMIT $%[5]d OFFSET $%[6]d
	`, rankExpr, whereFTS, visClause, extraWhere, limPos, offPos, repoOwnerNameExpr("u", "o"), repoOwnerJoin("r", "u", "o"))

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
	// LIMIT/OFFSET tail. Count query needs the owner join too when
	// OwnerFilter is set so the $N for username/slug resolves.
	ownerJoinForCount := ""
	if q.OwnerFilter != "" || q.RepoFilter != nil {
		ownerJoinForCount = repoOwnerJoin("r", "u", "o")
	}
	countQuery := fmt.Sprintf(`
		SELECT count(*)
		FROM repos_search rs
		JOIN repos r  ON r.id = rs.repo_id
		%[4]s
		WHERE %[1]s AND %[2]s %[3]s
	`, whereFTS, visClause, extraWhere, ownerJoinForCount)
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

// minIndexableTokenLen is the floor for a free-text query token to be
// likely-matchable by Postgres FTS. The english stemmer + index don't
// strip single-char tokens *syntactically*, but they almost never have
// a matching term in indexed content (no document contains lone "F").
// 3 was picked over 2 to still allow "go"/"ci"/"v1" — short but real.
const minIndexableTokenLen = 2

// validateFTSNotShortOnly returns ErrFTSStripped when every alphanumeric
// run in the raw free-text is shorter than minIndexableTokenLen. G11
// (F49): pre-fix `search issues "F"` returned 200 + empty, indistinct
// from a genuine no-match search; this surfaces a typed 422 the CLI
// translates to "try a longer or differently-worded query."
//
// Punctuation, whitespace, and non-letter/digit runes are token
// separators. A query like "F-audit" has token "audit" (length 5) and
// passes; a query like "F" or "F-" has only single-char tokens and
// fails.
func validateFTSNotShortOnly(text string) error {
	cur := 0
	for _, r := range text {
		if isTokenRune(r) {
			cur++
			if cur >= minIndexableTokenLen {
				return nil
			}
			continue
		}
		cur = 0
	}
	return ErrFTSStripped
}

func isTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
