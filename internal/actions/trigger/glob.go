// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import (
	"regexp"
	"strings"
	"sync"
)

// globCache memoizes the regex compilation per pattern. A workflow's
// `branches:` list compiles once and gets re-used across every
// candidate string (every changed path, every push event).
var globCache sync.Map

// matchAny evaluates a list of GHA-style filter patterns against a
// candidate string and returns whether the candidate is included.
//
// Pattern semantics (subset of minimatch — what GHA's filter spec
// guarantees and what `on.push.branches`/`on.pull_request.paths`/etc.
// fixtures actually use):
//
//   - Plain literal: `main` matches exactly "main".
//   - `*`           matches any sequence of non-`/` characters.
//   - `**`          matches any sequence including `/`.
//   - `/**` at end  matches zero or more trailing segments
//     (so `feature/**` matches both `feature` and
//     `feature/foo/bar`).
//   - `!pattern`    excludes. Evaluated in declaration order;
//     last-match wins. (Mirrors minimatch.)
//
// Empty pattern list returns true — "no filter" means "match all" per
// GHA convention.
//
// A list of *only* exclusions is treated as "include everything that
// doesn't match the exclusions" — without that branch, a
// `branches: [!main]` filter would reject every push.
func matchAny(patterns []string, s string) bool {
	if len(patterns) == 0 {
		return true
	}
	matched := false
	hasInclude := false
	for _, p := range patterns {
		if strings.HasPrefix(p, "!") {
			if globMatch(p[1:], s) {
				matched = false
			}
			continue
		}
		hasInclude = true
		if globMatch(p, s) {
			matched = true
		}
	}
	if !hasInclude {
		matched = true
		for _, p := range patterns {
			if strings.HasPrefix(p, "!") && globMatch(p[1:], s) {
				matched = false
			}
		}
	}
	return matched
}

// globMatch reports whether s matches the GHA-style pattern. The
// implementation translates the pattern to an anchored regex and
// memoizes the compile — patterns are repeatedly applied across many
// candidate strings (every changed path against a paths: filter).
func globMatch(pattern, s string) bool {
	re := compilePattern(pattern)
	return re.MatchString(s)
}

// compilePattern converts a GHA-style filter pattern into an anchored
// regex. Memoized via globCache so a workflow's repeated `branches:`
// list compiles once per process lifetime.
func compilePattern(pattern string) *regexp.Regexp {
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	expr := patternToRegex(pattern)
	re := regexp.MustCompile(expr)
	globCache.Store(pattern, re)
	return re
}

// patternToRegex translates a single pattern into the regex source.
// Order of cases matters: `/**` (path-optional suffix) → `**` →
// single `*` → escaped literal.
func patternToRegex(p string) string {
	var b strings.Builder
	b.Grow(len(p) + 16)
	b.WriteByte('^')
	for i := 0; i < len(p); {
		// `/**` — optional trailing path. Greedy match including zero
		// segments. Mirrors `feature/**` matching `feature` itself.
		if i+3 <= len(p) && p[i:i+3] == "/**" {
			b.WriteString(`(?:/.*)?`)
			i += 3
			continue
		}
		// `**/` — optional leading path. Mirrors `**/*.go` matching
		// `main.go` (zero leading segments). Also handles middle
		// occurrences like `docs/**/*.md` matching `docs/x.md`.
		if i+3 <= len(p) && p[i:i+3] == "**/" {
			b.WriteString(`(?:.*/)?`)
			i += 3
			continue
		}
		// `**` — match any sequence of any characters (greedy).
		if i+2 <= len(p) && p[i:i+2] == "**" {
			b.WriteString(`.*`)
			i += 2
			continue
		}
		// `*` — match any sequence of non-/ characters.
		if p[i] == '*' {
			b.WriteString(`[^/]*`)
			i++
			continue
		}
		// Literal byte. Escape if it's a regex metachar.
		c := p[i]
		if isRegexMeta(c) {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
		i++
	}
	b.WriteByte('$')
	return b.String()
}

func isRegexMeta(c byte) bool {
	switch c {
	case '.', '+', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$', '\\':
		return true
	}
	return false
}
