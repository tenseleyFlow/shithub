// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import "strings"

// ParsedQuery is the result of running a raw user-typed query
// through ParseQuery. The free-text portion is what flows into
// `plainto_tsquery` / `phraseto_tsquery`; the operator fields are
// what compose the SQL `WHERE` filters.
//
// `RepoFilter` carries the `owner/name` pair when the user typed
// `repo:owner/name`; both halves must be present for the filter
// to take effect (a bare `repo:foo` without slash is treated as
// free text).
type ParsedQuery struct {
	Text           string // free-text query (what tsvector matches against)
	Phrase         string // when a quoted phrase was supplied; empty when not
	RepoFilter     *RepoFilter
	StateFilter    string // "open" | "closed" | ""
	AuthorFilter   string // username or empty
	AssigneeFilter string // username or empty (issue_assignees join)
	// OwnerFilter matches `user:foo` and `org:foo` qualifiers — the
	// repo's owning user OR org slug. gh aliases the two; we mirror
	// that (E-audit E23). Bare form only — negation (`-org:foo`) is
	// not yet supported.
	OwnerFilter string
}

// RepoFilter splits the `repo:owner/name` operator value.
type RepoFilter struct {
	Owner string
	Name  string
}

// ParseQuery splits a raw query string into the free-text portion +
// recognised operators. v1 supports `repo:`, `is:`, `state:`,
// `author:`, `assignee:`, `user:`, `org:`. `is:`/`state:` and
// `user:`/`org:` are aliased.
//
// A quoted run of tokens becomes the Phrase field (one quoted span
// per query in v1). The Text field excludes quoted phrases and
// operator tokens.
//
// Operators and free-text tokens are space-delimited; the parser is
// intentionally tolerant — unknown prefixes (e.g. `language:Go`)
// fall through as free text so future operator additions don't
// break old queries.
func ParseQuery(raw string) ParsedQuery {
	out := ParsedQuery{}
	if raw == "" {
		return out
	}
	if len(raw) > MaxQueryBytes {
		raw = raw[:MaxQueryBytes]
	}

	// Pull quoted phrases out first. Single-pass: find the first
	// pair of "..." and treat that as the phrase. Anything else
	// quoted is treated as free text.
	if start := strings.IndexByte(raw, '"'); start >= 0 {
		end := strings.IndexByte(raw[start+1:], '"')
		if end > 0 {
			out.Phrase = strings.TrimSpace(raw[start+1 : start+1+end])
			raw = raw[:start] + " " + raw[start+1+end+1:]
		}
	}

	var freeText []string
	for _, tok := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(tok, "repo:"):
			val := strings.TrimPrefix(tok, "repo:")
			if i := strings.IndexByte(val, '/'); i > 0 && i < len(val)-1 {
				out.RepoFilter = &RepoFilter{Owner: val[:i], Name: val[i+1:]}
			} else {
				freeText = append(freeText, tok) // not owner/name shape — fall through
			}
		case strings.HasPrefix(tok, "is:"), strings.HasPrefix(tok, "state:"):
			val := strings.TrimPrefix(tok, "is:")
			val = strings.TrimPrefix(val, "state:")
			if val == "open" || val == "closed" {
				out.StateFilter = val
			} else {
				freeText = append(freeText, tok)
			}
		case strings.HasPrefix(tok, "author:"):
			val := strings.TrimPrefix(tok, "author:")
			if val != "" {
				out.AuthorFilter = val
			} else {
				freeText = append(freeText, tok)
			}
		case strings.HasPrefix(tok, "assignee:"):
			val := strings.TrimPrefix(tok, "assignee:")
			if val != "" {
				out.AssigneeFilter = val
			} else {
				freeText = append(freeText, tok)
			}
		case strings.HasPrefix(tok, "user:"), strings.HasPrefix(tok, "org:"):
			val := strings.TrimPrefix(tok, "user:")
			val = strings.TrimPrefix(val, "org:")
			if val != "" {
				out.OwnerFilter = val
			} else {
				freeText = append(freeText, tok)
			}
		default:
			freeText = append(freeText, tok)
		}
	}
	out.Text = strings.TrimSpace(strings.Join(freeText, " "))
	return out
}

// HasContent reports whether the parsed query contains anything
// searchable (free text, phrase, or any operator).
func (p ParsedQuery) HasContent() bool {
	return p.Text != "" || p.Phrase != "" || p.RepoFilter != nil ||
		p.StateFilter != "" || p.AuthorFilter != "" || p.AssigneeFilter != "" ||
		p.OwnerFilter != ""
}
