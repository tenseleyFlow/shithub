// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import "strings"

// Dialect controls the workflow file's accepted expression namespace.
//
// Default ("shithub"): the canonical namespace is `${{ shithub.* }}`.
// `${{ github.* }}` is accepted as an alias and the parser emits a
// Severity=Warning diagnostic so workflow authors are nudged to update.
//
// Strict: only `${{ shithub.* }}` is accepted; `${{ github.* }}`
// produces an error. Operators flip via cfg.Actions.DialectStrict
// when they want to forbid the alias outright (post-migration).
type Dialect string

const (
	DialectDefault Dialect = "shithub"
	DialectStrict  Dialect = "strict"
)

// NormalizeNamespace rewrites a `${{ … }}` body so any leading
// `github.` reference becomes `shithub.`. Returns the rewritten body
// plus a deprecated=true flag iff a rewrite occurred. The caller (the
// expression evaluator in S41a expr/eval.go) emits a Warning when
// deprecated=true && dialect == DialectDefault, or an Error when
// dialect == DialectStrict.
//
// We rewrite only at the namespace boundary (`github.` followed by
// `.event`, `.run_id`, etc.) — never inside string literals or
// arbitrary substrings. The expression evaluator does this token-aware;
// this helper is the simple form used by parse.go where we don't
// fully tokenize the value.
func NormalizeNamespace(body string) (rewritten string, deprecated bool) {
	// Quick path: the alias is rare; check before allocating.
	if !strings.Contains(body, "github.") {
		return body, false
	}
	// Walk tokens-ish: replace "github." preceded by a non-identifier
	// character (or start of string). This avoids rewriting inside
	// identifiers like `mygithub.foo`.
	var b strings.Builder
	i := 0
	for i < len(body) {
		// Find next "github." occurrence.
		idx := strings.Index(body[i:], "github.")
		if idx < 0 {
			b.WriteString(body[i:])
			break
		}
		abs := i + idx
		// Check the byte before — must be non-ident or start-of-string.
		if abs > 0 {
			c := body[abs-1]
			if isIdentChar(c) {
				// Bail on this occurrence: copy through and keep looking.
				b.WriteString(body[i : abs+len("github.")])
				i = abs + len("github.")
				continue
			}
		}
		b.WriteString(body[i:abs])
		b.WriteString("shithub.")
		i = abs + len("github.")
		deprecated = true
	}
	return b.String(), deprecated
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}
