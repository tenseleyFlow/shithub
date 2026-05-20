// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// SearchCode runs a code search across paths and content. Visibility
// gates the underlying repo set; only repos the actor can read appear.
//
// We run two unioned subqueries:
//
//	paths   — `tsv @@ plainto_tsquery(...)` on the indexed path
//	          string. Always populated (size cap doesn't apply).
//	content — `content_tsv @@ plainto_tsquery(...)` OR
//	          `content_trgm % $tsText` (trigram similarity for
//	          camelCase / snake_case identifiers).
//
// Path matches always rank above content matches at equal ts_rank.
// Within content matches, ts_rank dominates trigram similarity.
func SearchCode(ctx context.Context, deps Deps, actor policy.Actor, q ParsedQuery, limit, offset int) ([]CodeResult, int64, error) {
	if !q.HasContent() {
		return nil, 0, ErrEmptyQuery
	}
	tsText, tsCtor, hasFTS := tsQueryBindAndCtor(q)
	if !hasFTS && q.PathFilter == "" && q.ExtensionFilter == "" {
		// Code search needs a textual or path-like hit. repo:-only
		// narrows the repo set but we have nothing to match against;
		// return empty rather than blast every indexed file.
		return nil, 0, nil
	}

	args := []any{}
	tsPlaceholder := 0
	if hasFTS {
		// G11 (F49): code search requires a textual hit to be long
		// enough to plausibly match indexed content. Path/extension-only
		// queries are explicit scoped lookups and skip this guard.
		if err := validateFTSNotShortOnly(tsText); err != nil {
			return nil, 0, err
		}
		args = append(args, tsText)
		tsPlaceholder = len(args)
	}
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", len(args)+1)
	args = append(args, visArgs...)

	scopeFilter := ""
	if q.RepoFilter != nil {
		ownerPos := len(args) + 1
		namePos := len(args) + 2
		args = append(args, q.RepoFilter.Owner, q.RepoFilter.Name)
		scopeFilter += repoFilterByOwnerName("r", ownerPos, namePos)
	}
	if q.OwnerFilter != "" {
		ownerPos := len(args) + 1
		args = append(args, q.OwnerFilter)
		scopeFilter += fmt.Sprintf(
			" AND (u.username = $%d OR o.slug = $%d)",
			ownerPos, ownerPos,
		)
	}
	scopeFilter += appendCITextFilter(&args, "coalesce(r.primary_language, '')", q.LanguageFilter)
	scopeFilter += appendRepoQualifierFilters(&args, "r", q)

	pathFilter := scopeFilter
	pathFilter += appendCILikeFilter(&args, "csp.path", q.PathFilter)
	pathFilter += appendCISuffixFilter(&args, "csp.path", q.ExtensionFilter)

	contentFilter := scopeFilter
	contentFilter += appendCILikeFilter(&args, "csc.path", q.PathFilter)
	contentFilter += appendCISuffixFilter(&args, "csc.path", q.ExtensionFilter)

	limPos := len(args) + 1
	offPos := len(args) + 2
	args = append(args, limit, offset)

	// Path subquery: tsv match on the path string. We always rank
	// path hits at +1.0 above content hits at the same ts_rank.
	pathWhereFTS := "TRUE"
	pathRank := "1.0"
	contentWhereFTS := "FALSE"
	contentRank := "0.0"
	if hasFTS {
		pathWhereFTS = fmt.Sprintf("csp.tsv @@ %[1]s('shithub_search', $%[2]d)", tsCtor, tsPlaceholder)
		pathRank = fmt.Sprintf("ts_rank_cd(csp.tsv, %[1]s('shithub_search', $%[2]d)) + 1.0", tsCtor, tsPlaceholder)
		contentWhereFTS = fmt.Sprintf("csc.content_tsv @@ %[1]s('shithub_search', $%[2]d)", tsCtor, tsPlaceholder)
		contentRank = fmt.Sprintf("ts_rank_cd(csc.content_tsv, %[1]s('shithub_search', $%[2]d))", tsCtor, tsPlaceholder)
	}

	queryStr := fmt.Sprintf(`
		WITH path_hits AS (
		    SELECT csp.repo_id, csp.ref_name, csp.path,
		           %[1]s AS rank,
		           ''::text AS preview
		    FROM code_search_paths csp
		    JOIN repos r ON r.id = csp.repo_id
		    %[9]s
		    WHERE %[2]s
		      AND %[3]s
		      %[4]s
		),
		content_hits AS (
		    SELECT csc.repo_id, csc.ref_name, csc.path,
		           %[5]s AS rank,
		           ''::text AS preview
		    FROM code_search_content csc
		    JOIN repos r ON r.id = csc.repo_id
		    %[9]s
		    WHERE %[6]s
		      AND %[3]s
		      %[7]s
		),
		all_hits AS (
		    SELECT * FROM path_hits
		    UNION ALL
		    SELECT * FROM content_hits
		)
		SELECT h.repo_id, %[8]s, r.name, h.ref_name, h.path, h.preview, h.rank
		FROM all_hits h
		JOIN repos r ON r.id = h.repo_id
		%[12]s
		ORDER BY h.rank DESC, h.path
		LIMIT $%[10]d OFFSET $%[11]d
	`, pathRank, pathWhereFTS, visClause, pathFilter, contentRank, contentWhereFTS, contentFilter, repoOwnerNameExpr("u", "o"), repoOwnerJoin("r", "u", "o"), limPos, offPos, repoOwnerJoin("r", "u", "o"))

	rows, err := deps.Pool.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search code: %w", err)
	}
	defer rows.Close()
	out := make([]CodeResult, 0, limit)
	for rows.Next() {
		var r CodeResult
		if err := rows.Scan(&r.RepoID, &r.OwnerUsername, &r.RepoName,
			&r.RefName, &r.Path, &r.PreviewLine, &r.Rank); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Total count: paths + content rows that matched. Repos with
	// visibility filter applied. Pagination is approximate when
	// the same path matches both indexes — we count the union
	// honestly so the pager doesn't lie.
	countQuery := fmt.Sprintf(`
		SELECT (
		    SELECT count(*) FROM code_search_paths csp
		    JOIN repos r ON r.id = csp.repo_id
		    %[6]s
		    WHERE %[1]s
		      AND %[2]s
		      %[3]s
		) + (
		    SELECT count(*) FROM code_search_content csc
		    JOIN repos r ON r.id = csc.repo_id
		    %[6]s
		    WHERE %[4]s
		      AND %[2]s
		      %[5]s
		)
	`, pathWhereFTS, visClause, pathFilter, contentWhereFTS, contentFilter, repoOwnerJoin("r", "u", "o"))
	var total int64
	if err := deps.Pool.QueryRow(ctx, countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count code: %w", err)
	}
	return out, total, nil
}
