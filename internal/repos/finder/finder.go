// SPDX-License-Identifier: AGPL-3.0-or-later

// Package finder implements the "Go to file" fuzzy match used by the
// /find/{ref} endpoint. The matcher is a simple subsequence scorer
// with bonuses for path-segment boundaries — close enough to feel like
// VS Code's quickopen for a few thousand entries without pulling a
// full fuzzy library.
package finder

import (
	"sort"
	"strings"
	"unicode"
)

// Match is one row in the finder result list.
type Match struct {
	Path  string
	Score int
}

// Filter returns the top `limit` matches against query, scored highest
// first. A blank query returns the first `limit` paths in input order.
//
// The matcher is case-insensitive and rewards:
//   - consecutive characters (longer runs score higher)
//   - matches at path-segment starts (after `/` or at index 0)
//   - matches at filename basename
//
// Designed for input sizes up to ~50k paths; past that, restrict the
// callable surface or paginate.
func Filter(paths []string, query string, limit int) []Match {
	q := strings.TrimSpace(query)
	if q == "" {
		out := make([]Match, 0, min(limit, len(paths)))
		for i, p := range paths {
			if i >= limit {
				break
			}
			out = append(out, Match{Path: p, Score: 0})
		}
		return out
	}
	qLower := []rune(strings.ToLower(q))
	matches := make([]Match, 0, 64)
	for _, p := range paths {
		if score, ok := score(p, qLower); ok {
			matches = append(matches, Match{Path: p, Score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		// Tiebreaker: shorter paths first (more specific matches).
		if len(matches[i].Path) != len(matches[j].Path) {
			return len(matches[i].Path) < len(matches[j].Path)
		}
		return matches[i].Path < matches[j].Path
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// score is a single subsequence-with-bonus pass. Returns (score, true)
// when every query rune is consumed in order; otherwise (0, false).
func score(path string, q []rune) (int, bool) {
	if len(q) == 0 {
		return 0, true
	}
	pLower := []rune(strings.ToLower(path))
	score := 0
	qi := 0
	prevMatchedAt := -2 // forces "consecutive" check to fail on first hit
	for i := 0; i < len(pLower) && qi < len(q); i++ {
		if pLower[i] != q[qi] {
			continue
		}
		// Base hit.
		score += 1
		// Consecutive run bonus.
		if i == prevMatchedAt+1 {
			score += 4
		}
		// Boundary bonus: start of path or after `/`.
		if i == 0 || pLower[i-1] == '/' {
			score += 6
		}
		// Camel/kebab boundary: lowercase-after-uppercase is a weaker
		// boundary than `/`, but worth a smaller bonus.
		if i > 0 && unicode.IsUpper(rune(path[i])) && unicode.IsLower(rune(path[i-1])) {
			score += 3
		}
		prevMatchedAt = i
		qi++
	}
	if qi != len(q) {
		return 0, false
	}
	// Filename-basename bonus: query that fully matches the basename
	// gets a kicker so `repo.go` ranks above `repo_settings_form.html`.
	if base := basename(path); strings.Contains(strings.ToLower(base), string(q)) {
		score += 8
	}
	return score, true
}

// basename returns the path's last segment (no leading slash).
func basename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
