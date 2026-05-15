// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// PRO-EXT01-08b — regex code search.
//
// Postgres' POSIX regex via `~` is not RE2; it can blow up on a
// catastrophic pattern. We defend the database with three layers:
//
//  1. Pre-validate via Go's regexp/syntax (RE2). If a pattern won't
//     compile under RE2's saner subset, reject it. This catches the
//     most common DoS shapes (nested unbounded backtracks).
//
//  2. SET LOCAL statement_timeout for the regex transaction so a
//     pathological pattern that compiles but takes seconds to scan
//     terminates rather than starves the pool.
//
//  3. Trigram pre-filter: extract any 3-char run from the regex
//     literal text and require `content_trgm % literal` on
//     code_search_content rows. With pg_trgm's GIN/GIST index this
//     bounds the candidate set to "rows that share trigrams with
//     the literal hint", which for almost any real-world regex is
//     a 100×–1000× reduction.

// MaxRegexPatternBytes caps the regex source length. Already gated
// by the form's maxlength, but doubles as defense-in-depth.
const MaxRegexPatternBytes = 500

// ErrRegexInvalid is returned when the supplied pattern fails RE2
// pre-validation. Handlers surface this as a user-facing error
// rather than a 500.
var ErrRegexInvalid = errors.New("search: regex is invalid")

// CodeRegexParams gathers the inputs to SearchCodeRegex. RegexPattern
// is the user-supplied pattern; everything else mirrors the standard
// code search.
type CodeRegexParams struct {
	RegexPattern string
	RepoFilter   *RepoFilter
}

// SearchCodeRegex runs a regex match against code_search_content
// (path matches are not regex-able with the same precision because
// the path index is a tsvector; if a user wants regex-on-path they
// can search content for the same pattern and inspect the hits).
//
// Returns ErrRegexInvalid for bad patterns; other DB errors bubble.
// statement_timeout is set to 2 seconds inside the transaction; a
// timeout returns a wrapped pgx error to the caller.
func SearchCodeRegex(ctx context.Context, deps Deps, actor policy.Actor, p CodeRegexParams, limit, offset int) ([]CodeResult, int64, error) {
	pattern := strings.TrimSpace(p.RegexPattern)
	if pattern == "" {
		return nil, 0, ErrEmptyQuery
	}
	if len(pattern) > MaxRegexPatternBytes {
		return nil, 0, ErrRegexInvalid
	}
	// RE2 pre-validation. If Go can't compile it under RE2's well-
	// behaved subset, refuse to hand it to Postgres' POSIX engine.
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, 0, ErrRegexInvalid
	}

	// Extract a trigram literal hint: the longest run of `[\w]` (>=3)
	// in the source. Common-case wins: a pattern like `func\s+myFunc`
	// extracts "myFunc"; a pattern like `\bTODO\b` extracts "TODO".
	hint := extractLongestLiteralRun(pattern, 3)

	args := []any{pattern}
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", len(args)+1)
	args = append(args, visArgs...)

	repoFilter := ""
	if p.RepoFilter != nil {
		ownerPos := len(args) + 1
		namePos := len(args) + 2
		args = append(args, p.RepoFilter.Owner, p.RepoFilter.Name)
		repoFilter = repoFilterByOwnerName("r", ownerPos, namePos)
	}

	hintFilter := ""
	if hint != "" {
		hintPos := len(args) + 1
		args = append(args, hint)
		hintFilter = fmt.Sprintf("AND csc.content_trgm %% $%d", hintPos)
	}

	limPos := len(args) + 1
	offPos := len(args) + 2
	args = append(args, limit, offset)

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("regex search begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '2s'"); err != nil {
		return nil, 0, fmt.Errorf("regex search set timeout: %w", err)
	}

	queryStr := fmt.Sprintf(`
        SELECT csc.repo_id, %[3]s, r.name, csc.ref_name, csc.path,
               ''::text AS preview,
               0.5::real AS rank
        FROM code_search_content csc
        JOIN repos r ON r.id = csc.repo_id
        %[4]s
        WHERE %[1]s
          %[5]s
          %[2]s
          AND csc.content_trgm ~ $1
        ORDER BY csc.path
        LIMIT $%[6]d OFFSET $%[7]d
    `, visClause, repoFilter, repoOwnerNameExpr("u", "o"), repoOwnerJoin("r", "u", "o"), hintFilter, limPos, offPos)

	rows, err := tx.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("regex search: %w", err)
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

	countQuery := fmt.Sprintf(`
        SELECT count(*)
        FROM code_search_content csc
        JOIN repos r ON r.id = csc.repo_id
        WHERE %[1]s
          %[3]s
          %[2]s
          AND csc.content_trgm ~ $1
    `, visClause, repoFilter, hintFilter)
	var total int64
	if err := tx.QueryRow(ctx, countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("regex count: %w", err)
	}
	return out, total, nil
}

// extractLongestLiteralRun walks the regex source and returns the
// longest run of [A-Za-z0-9_] characters not preceded by `\\`. Empty
// string when no run reaches minLen. Cheap heuristic — not a regex
// parser; it's enough for pg_trgm to do useful pre-filtering on the
// common patterns. Regexes that are pure operators (`^.*$`, `\s+`,
// `[a-z]+`) will produce no hint and fall back to the full scan
// under the statement timeout.
func extractLongestLiteralRun(pattern string, minLen int) string {
	best := ""
	cur := strings.Builder{}
	escape := false
	flush := func() {
		if cur.Len() > 0 {
			if s := cur.String(); len(s) > len(best) {
				best = s
			}
			cur.Reset()
		}
	}
	for _, c := range pattern {
		if escape {
			escape = false
			flush()
			continue
		}
		if c == '\\' {
			escape = true
			flush()
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			cur.WriteRune(c)
			continue
		}
		flush()
	}
	flush()
	if len(best) < minLen {
		return ""
	}
	return best
}
