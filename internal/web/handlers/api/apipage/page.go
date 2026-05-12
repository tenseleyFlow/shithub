// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apipage centralizes /api/v1 list-endpoint pagination concerns:
// reading page / per_page query params with sensible defaults and clamps,
// and emitting the canonical RFC 8288 Link header for cursor navigation.
//
// The Link header format matches GitHub's REST API verbatim so existing
// gh-style clients (including shithub-cli/internal/api.ParseLinkHeader)
// keep working. All emitted URLs are absolute when a baseURL is provided;
// callers should pass their configured public base URL so links survive
// reverse-proxying and host-rewriting.
package apipage

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPerPage is the per_page value used when the caller omits it.
const DefaultPerPage = 30

// MaxPerPage caps per_page to prevent unbounded list responses. Mirrors
// GitHub REST's 100 ceiling so client expectations port directly.
const MaxPerPage = 100

// Page describes a paginated response state. Total >= 0 enables emitting
// first/last in the Link header. Total == -1 disables them and falls
// back to HasMore for forward-only pagination (used when totals are
// expensive to compute and a "next" cursor is cheap).
type Page struct {
	Current int  // 1-indexed; must be >= 1
	PerPage int  // > 0
	Total   int  // total items across all pages; -1 when unknown
	HasMore bool // honored only when Total < 0
}

// ParseQuery reads ?page= and ?per_page= from r.URL.Query() with
// defaults page=1, per_page=defaultPerPage. per_page is clamped to
// [1, maxPerPage]. Non-integer or negative values fall back to defaults
// rather than 400 — matches gh/GitHub leniency for list endpoints.
func ParseQuery(r *http.Request, defaultPerPage, maxPerPage int) (page, perPage int) {
	if defaultPerPage <= 0 {
		defaultPerPage = DefaultPerPage
	}
	if maxPerPage <= 0 {
		maxPerPage = MaxPerPage
	}
	q := r.URL.Query()
	page = atoiOr(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage = atoiOr(q.Get("per_page"), defaultPerPage)
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// LinkHeader returns the canonical Link header value for p. The header
// is composed of up to four entries (first, prev, next, last) with
// rel values quoted per RFC 8288.
//
// baseURL is the public scheme://host prefix (e.g. "https://shithub.sh");
// when empty, links are emitted as path-relative URLs. reqURL is the
// incoming request URL — its query string is preserved and only the
// page parameter is rewritten per rel.
//
// Returns "" when there is no useful link to emit (single-page result
// with no forward signal).
func (p Page) LinkHeader(baseURL string, reqURL *url.URL) string {
	if reqURL == nil || p.PerPage <= 0 {
		return ""
	}
	cur := p.Current
	if cur < 1 {
		cur = 1
	}

	var lastPage int
	knownTotal := p.Total >= 0
	if knownTotal {
		lastPage = lastPageFor(p.Total, p.PerPage)
	}

	var hasPrev, hasNext bool
	switch {
	case knownTotal:
		hasPrev = cur > 1 && lastPage >= 1
		hasNext = cur < lastPage
	default:
		hasPrev = cur > 1
		hasNext = p.HasMore
	}

	if !hasPrev && !hasNext && !knownTotal {
		return ""
	}
	if knownTotal && lastPage <= 1 {
		return ""
	}

	prefix := strings.TrimRight(baseURL, "/")

	var entries []string
	if knownTotal {
		entries = append(entries, formatLink(prefix, reqURL, 1, "first"))
	}
	if hasPrev {
		entries = append(entries, formatLink(prefix, reqURL, cur-1, "prev"))
	}
	if hasNext {
		entries = append(entries, formatLink(prefix, reqURL, cur+1, "next"))
	}
	if knownTotal && lastPage >= 1 {
		entries = append(entries, formatLink(prefix, reqURL, lastPage, "last"))
	}
	return strings.Join(entries, ", ")
}

// lastPageFor returns the 1-indexed page count for a known total. Always
// >= 1 so callers can render "first" / "last" even on an empty result.
func lastPageFor(total, perPage int) int {
	if total <= 0 {
		return 1
	}
	pages := total / perPage
	if total%perPage != 0 {
		pages++
	}
	if pages < 1 {
		pages = 1
	}
	return pages
}

func formatLink(prefix string, reqURL *url.URL, page int, rel string) string {
	q := reqURL.Query()
	q.Set("page", strconv.Itoa(page))
	rebuilt := *reqURL
	rebuilt.RawQuery = q.Encode()
	rebuilt.Scheme = ""
	rebuilt.Host = ""

	var b strings.Builder
	b.WriteByte('<')
	if prefix != "" {
		b.WriteString(prefix)
	}
	b.WriteString(rebuilt.RequestURI())
	b.WriteString(`>; rel="`)
	b.WriteString(rel)
	b.WriteByte('"')
	return b.String()
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
