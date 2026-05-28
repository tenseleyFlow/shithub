// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"strings"
	"time"
)

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
	Text                  string // free-text query (what tsvector matches against)
	Phrase                string // when a quoted phrase was supplied; empty when not
	Terms                 []TextTerm
	ExcludedTerms         []TextTerm
	Qualifiers            []Qualifier
	RepoFilter            *RepoFilter
	StateFilter           string // "open" | "closed" | ""
	KindFilter            string // "issue" | "pr" | ""
	MergedStateFilter     string // "merged" | "unmerged" | ""
	LockedFilter          *bool
	AuthorFilter          string // username or empty
	AssigneeFilter        string // username or empty (issue_assignees join)
	AssigneeAnyFilter     bool   // assignee:* (at least one assignee)
	CommenterFilter       string // username or empty (issue_comments join)
	MentionFilter         string // username mentioned in issue body or comments
	InvolvesFilters       []string
	ReviewRequestedFilter string   // username with an active PR review request
	MissingFilters        []string // label | milestone | assignee | project
	SortFilter            string   // normalized GitHub-style sort qualifier
	// OwnerFilter matches `user:foo` and `org:foo` qualifiers — the
	// repo's owning user OR org slug. gh aliases them for search; we
	// keep the positive fast path here and retain negated forms in
	// Qualifiers for callers that can enforce exclusions.
	OwnerFilter      string
	LabelFilters     []string
	MilestoneFilter  string
	LanguageFilter   string
	VisibilityFilter string
	ForkFilter       *bool
	ArchivedFilter   *bool
	TopicFilters     []string
	PathFilter       string
	ExtensionFilter  string
	CreatedFilter    *DateRange
	UpdatedFilter    *DateRange
	ClosedFilter     *DateRange
	MergedFilter     *DateRange
}

// RepoFilter splits the `repo:owner/name` operator value.
type RepoFilter struct {
	Owner string
	Name  string
}

// TextTerm is a free-text term from a parsed query. Quoted terms keep
// Phrase=true so SQL callers can choose phrase matching where the
// backing index supports it. Negated terms are retained for callers
// that can enforce exclusions; legacy FTS callers ignore them instead
// of accidentally broadening the query.
type TextTerm struct {
	Value   string
	Phrase  bool
	Negated bool
}

// Qualifier is the normalized form of a recognized `key:value`
// operator. Specific high-traffic qualifiers are also projected onto
// ParsedQuery fields for the existing search execution paths.
type Qualifier struct {
	Key     string
	Value   string
	Negated bool
}

// DateRange is a normalized ISO-date/date-range qualifier. Bounds are
// half-open: From is inclusive, To is exclusive. An exact date such as
// `created:2026-05-19` becomes [2026-05-19T00:00Z, 2026-05-20T00:00Z).
type DateRange struct {
	From    time.Time
	To      time.Time
	HasFrom bool
	HasTo   bool
}

// ParseQuery splits a raw query string into free-text terms plus
// recognized GitHub-style qualifiers. It is intentionally tolerant:
// malformed or unknown operators fall back to free text so old queries
// keep working while the grammar grows.
func ParseQuery(raw string) ParsedQuery {
	out := ParsedQuery{}
	if raw == "" {
		return out
	}
	if len(raw) > MaxQueryBytes {
		raw = raw[:MaxQueryBytes]
	}

	var freeText []string
	for _, tok := range scanQueryTokens(raw) {
		if tok.Value == "" {
			continue
		}
		key, val, ok := splitQualifier(tok.Value)
		if ok {
			if applyQualifier(&out, key, val, tok.Negated) {
				continue
			}
		}

		term := TextTerm{Value: tok.Value, Phrase: tok.Quoted, Negated: tok.Negated}
		if tok.Negated {
			out.ExcludedTerms = append(out.ExcludedTerms, term)
			continue
		}
		out.Terms = append(out.Terms, term)
		if tok.Quoted && out.Phrase == "" {
			out.Phrase = tok.Value
		} else {
			freeText = append(freeText, tok.Value)
		}
	}
	out.Text = strings.TrimSpace(strings.Join(freeText, " "))
	return out
}

type queryToken struct {
	Value   string
	Quoted  bool
	Negated bool
}

func scanQueryTokens(raw string) []queryToken {
	tokens := []queryToken{}
	for i := 0; i < len(raw); {
		for i < len(raw) && isSearchSpace(raw[i]) {
			i++
		}
		if i >= len(raw) {
			break
		}

		negated := false
		if raw[i] == '-' {
			negated = true
			i++
		}

		var b strings.Builder
		quoted := false
		inQuote := false
		for i < len(raw) {
			ch := raw[i]
			if ch == '"' {
				quoted = true
				inQuote = !inQuote
				i++
				continue
			}
			if !inQuote && isSearchSpace(ch) {
				break
			}
			if inQuote && ch == '\\' && i+1 < len(raw) {
				i++
				b.WriteByte(raw[i])
				i++
				continue
			}
			b.WriteByte(ch)
			i++
		}
		if value := strings.TrimSpace(b.String()); value != "" {
			tokens = append(tokens, queryToken{Value: value, Quoted: quoted, Negated: negated})
		}
	}
	return tokens
}

func isSearchSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

func splitQualifier(value string) (key, val string, ok bool) {
	i := strings.IndexByte(value, ':')
	if i <= 0 || i == len(value)-1 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(value[:i]))
	val = strings.TrimSpace(value[i+1:])
	return key, val, key != "" && val != ""
}

func applyQualifier(out *ParsedQuery, key, val string, negated bool) bool {
	switch key {
	case "repo", "is", "type", "state", "author", "assignee", "commenter",
		"mentions", "involves", "review-requested", "sort", "no",
		"user", "org", "label", "milestone", "language", "path",
		"extension", "created", "updated", "closed", "merged",
		"visibility", "fork", "archived", "topic":
	default:
		return false
	}

	if negated {
		out.Qualifiers = append(out.Qualifiers, Qualifier{Key: key, Value: val, Negated: true})
		return true
	}

	switch key {
	case "repo":
		if i := strings.IndexByte(val, '/'); i > 0 && i < len(val)-1 {
			out.RepoFilter = &RepoFilter{Owner: val[:i], Name: val[i+1:]}
		} else {
			return false
		}
	case "is":
		switch strings.ToLower(val) {
		case "open", "closed":
			out.StateFilter = strings.ToLower(val)
		case "issue":
			out.KindFilter = "issue"
		case "pr", "pull-request", "pull_request":
			out.KindFilter = "pr"
		case "merged":
			out.MergedStateFilter = "merged"
		case "unmerged":
			out.MergedStateFilter = "unmerged"
		case "locked":
			out.LockedFilter = boolSearchPtr(true)
		case "unlocked":
			out.LockedFilter = boolSearchPtr(false)
		case "public", "private":
			out.VisibilityFilter = strings.ToLower(val)
		case "fork":
			out.ForkFilter = boolSearchPtr(true)
		case "archived":
			out.ArchivedFilter = boolSearchPtr(true)
		default:
			return false
		}
	case "type":
		switch strings.ToLower(val) {
		case "issue":
			out.KindFilter = "issue"
		case "pr", "pull-request", "pull_request":
			out.KindFilter = "pr"
		default:
			return false
		}
	case "state":
		switch strings.ToLower(val) {
		case "open", "closed":
			out.StateFilter = strings.ToLower(val)
		default:
			return false
		}
	case "author":
		out.AuthorFilter = val
	case "assignee":
		if val == "*" {
			out.AssigneeAnyFilter = true
		} else {
			out.AssigneeFilter = val
		}
	case "commenter":
		out.CommenterFilter = val
	case "mentions":
		out.MentionFilter = val
	case "involves":
		out.InvolvesFilters = append(out.InvolvesFilters, val)
	case "review-requested":
		out.ReviewRequestedFilter = val
	case "sort":
		sort, ok := normalizeSortQualifier(val)
		if !ok {
			return false
		}
		out.SortFilter = sort
	case "no":
		switch strings.ToLower(val) {
		case "label", "milestone", "assignee", "project":
			out.MissingFilters = append(out.MissingFilters, strings.ToLower(val))
		default:
			return false
		}
	case "user", "org":
		out.OwnerFilter = val
	case "label":
		out.LabelFilters = append(out.LabelFilters, val)
	case "milestone":
		out.MilestoneFilter = val
	case "language":
		out.LanguageFilter = val
	case "visibility":
		switch strings.ToLower(val) {
		case "public", "private":
			out.VisibilityFilter = strings.ToLower(val)
		default:
			return false
		}
	case "fork":
		v, ok := parseBoolSearchQualifier(val)
		if !ok {
			return false
		}
		out.ForkFilter = boolSearchPtr(v)
	case "archived":
		v, ok := parseBoolSearchQualifier(val)
		if !ok {
			return false
		}
		out.ArchivedFilter = boolSearchPtr(v)
	case "topic":
		out.TopicFilters = append(out.TopicFilters, val)
	case "path":
		out.PathFilter = val
	case "extension":
		out.ExtensionFilter = strings.TrimPrefix(val, ".")
	case "created", "updated", "closed", "merged":
		dr, ok := parseDateRange(val)
		if !ok {
			return false
		}
		switch key {
		case "created":
			out.CreatedFilter = &dr
		case "updated":
			out.UpdatedFilter = &dr
		case "closed":
			out.ClosedFilter = &dr
		case "merged":
			out.MergedFilter = &dr
		}
	}
	out.Qualifiers = append(out.Qualifiers, Qualifier{Key: key, Value: val})
	return true
}

func normalizeSortQualifier(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "comments":
		return "comments-desc", true
	case "comments-asc", "comments-desc":
		return value, true
	case "created":
		return "created-desc", true
	case "created-asc", "created-desc":
		return value, true
	case "updated":
		return "updated-desc", true
	case "updated-asc", "updated-desc":
		return value, true
	case "relevance":
		return "relevance-desc", true
	case "relevance-asc", "relevance-desc":
		return value, true
	default:
		return "", false
	}
}

func boolSearchPtr(v bool) *bool {
	return &v
}

func parseBoolSearchQualifier(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "1", "only":
		return true, true
	case "false", "no", "0":
		return false, true
	default:
		return false, false
	}
}

func parseDateRange(raw string) (DateRange, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DateRange{}, false
	}
	var out DateRange
	switch {
	case strings.HasPrefix(value, ">="):
		t, ok := parseISODate(value[2:])
		if !ok {
			return DateRange{}, false
		}
		out.From, out.HasFrom = t, true
	case strings.HasPrefix(value, ">"):
		t, ok := parseISODate(value[1:])
		if !ok {
			return DateRange{}, false
		}
		out.From, out.HasFrom = t.AddDate(0, 0, 1), true
	case strings.HasPrefix(value, "<="):
		t, ok := parseISODate(value[2:])
		if !ok {
			return DateRange{}, false
		}
		out.To, out.HasTo = t.AddDate(0, 0, 1), true
	case strings.HasPrefix(value, "<"):
		t, ok := parseISODate(value[1:])
		if !ok {
			return DateRange{}, false
		}
		out.To, out.HasTo = t, true
	case strings.Contains(value, ".."):
		parts := strings.SplitN(value, "..", 2)
		if parts[0] != "" {
			t, ok := parseISODate(parts[0])
			if !ok {
				return DateRange{}, false
			}
			out.From, out.HasFrom = t, true
		}
		if parts[1] != "" {
			t, ok := parseISODate(parts[1])
			if !ok {
				return DateRange{}, false
			}
			out.To, out.HasTo = t.AddDate(0, 0, 1), true
		}
		if !out.HasFrom && !out.HasTo {
			return DateRange{}, false
		}
	default:
		t, ok := parseISODate(value)
		if !ok {
			return DateRange{}, false
		}
		out.From, out.HasFrom = t, true
		out.To, out.HasTo = t.AddDate(0, 0, 1), true
	}
	return out, true
}

func parseISODate(raw string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// HasContent reports whether the parsed query contains anything
// searchable (free text, phrase, or any operator).
func (p ParsedQuery) HasContent() bool {
	return p.Text != "" || p.Phrase != "" || p.RepoFilter != nil ||
		p.StateFilter != "" || p.KindFilter != "" || p.AuthorFilter != "" ||
		p.MergedStateFilter != "" || p.LockedFilter != nil ||
		p.AssigneeFilter != "" || p.AssigneeAnyFilter || p.CommenterFilter != "" ||
		p.MentionFilter != "" || len(p.InvolvesFilters) > 0 ||
		p.ReviewRequestedFilter != "" || p.SortFilter != "" ||
		len(p.MissingFilters) > 0 || p.OwnerFilter != "" ||
		len(p.LabelFilters) > 0 || p.MilestoneFilter != "" ||
		p.LanguageFilter != "" || p.VisibilityFilter != "" || p.ForkFilter != nil ||
		p.ArchivedFilter != nil || len(p.TopicFilters) > 0 ||
		p.PathFilter != "" || p.ExtensionFilter != "" ||
		p.CreatedFilter != nil || p.UpdatedFilter != nil || p.ClosedFilter != nil ||
		p.MergedFilter != nil || len(p.ExcludedTerms) > 0 || len(p.Qualifiers) > 0
}
