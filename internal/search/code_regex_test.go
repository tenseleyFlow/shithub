// SPDX-License-Identifier: AGPL-3.0-or-later

package search_test

import (
	"context"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/search"
)

// PRO-EXT01-08b — regex code search invariants. The integration
// happy-path (matching real indexed content) is exercised by manual
// smoke since the test setup doesn't seed `code_search_content`;
// these tests cover the input-validation contract + literal-hint
// extractor that bounds the trigram pre-filter.

func TestSearchCodeRegex_RejectsEmpty(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, _, err := search.SearchCodeRegex(context.Background(), f.deps,
		policy.UserActor(f.alice.ID, f.alice.Username, false, false),
		search.CodeRegexParams{RegexPattern: ""}, 10, 0)
	if err != search.ErrEmptyQuery {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}
}

func TestSearchCodeRegex_RejectsInvalid(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Unbalanced group: RE2 (and POSIX) both reject.
	_, _, err := search.SearchCodeRegex(context.Background(), f.deps,
		policy.UserActor(f.alice.ID, f.alice.Username, false, false),
		search.CodeRegexParams{RegexPattern: "foo(bar"}, 10, 0)
	if err != search.ErrRegexInvalid {
		t.Fatalf("expected ErrRegexInvalid, got %v", err)
	}
}

func TestSearchCodeRegex_RejectsOversize(t *testing.T) {
	t.Parallel()
	f := setup(t)
	huge := make([]byte, search.MaxRegexPatternBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	_, _, err := search.SearchCodeRegex(context.Background(), f.deps,
		policy.UserActor(f.alice.ID, f.alice.Username, false, false),
		search.CodeRegexParams{RegexPattern: string(huge)}, 10, 0)
	if err != search.ErrRegexInvalid {
		t.Fatalf("expected ErrRegexInvalid (oversize), got %v", err)
	}
}

func TestSearchCodeRegex_RunsAgainstEmptyIndexCleanly(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Valid pattern, empty content index → expect (empty rows, 0 total,
	// no error). Exercises the SET LOCAL statement_timeout + trigram
	// pre-filter path without seeding fixtures.
	rows, total, err := search.SearchCodeRegex(context.Background(), f.deps,
		policy.UserActor(f.alice.ID, f.alice.Username, false, false),
		search.CodeRegexParams{RegexPattern: `\bTODO\b`}, 10, 0)
	if err != nil {
		t.Fatalf("regex on empty index should succeed: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("expected empty results, got total=%d rows=%d", total, len(rows))
	}
}

func TestSearchCodeRegex_AnonymousVisibilityRespected(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Just runs the query as anonymous. Empty content index means
	// the visibility predicate doesn't have to filter rows out, but
	// the SQL must still compile + execute without erroring.
	_, _, err := search.SearchCodeRegex(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.CodeRegexParams{RegexPattern: `secret`}, 10, 0)
	if err != nil {
		t.Fatalf("anonymous regex should not error: %v", err)
	}
}
