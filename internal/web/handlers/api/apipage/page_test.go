// SPDX-License-Identifier: AGPL-3.0-or-later

package apipage_test

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
)

func TestParseQuery_Defaults(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api/v1/things", nil)
	page, perPage := apipage.ParseQuery(r, 0, 0)
	if page != 1 || perPage != apipage.DefaultPerPage {
		t.Fatalf("got page=%d, perPage=%d; want 1/%d", page, perPage, apipage.DefaultPerPage)
	}
}

func TestParseQuery_ClampsPerPage(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api/v1/things?page=2&per_page=500", nil)
	page, perPage := apipage.ParseQuery(r, 30, 100)
	if page != 2 {
		t.Errorf("page: got %d, want 2", page)
	}
	if perPage != 100 {
		t.Errorf("per_page: got %d, want 100", perPage)
	}
}

func TestParseQuery_NegativeFallback(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api/v1/things?page=-3&per_page=-1", nil)
	page, perPage := apipage.ParseQuery(r, 0, 0)
	if page != 1 || perPage != apipage.DefaultPerPage {
		t.Fatalf("got page=%d, perPage=%d; want 1/%d", page, perPage, apipage.DefaultPerPage)
	}
}

func TestParseQuery_NonInteger(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api/v1/things?page=banana&per_page=cherry", nil)
	page, perPage := apipage.ParseQuery(r, 25, 50)
	if page != 1 || perPage != 25 {
		t.Fatalf("got page=%d, perPage=%d; want 1/25", page, perPage)
	}
}

func TestLinkHeader_MiddlePage(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/user/starred?per_page=30&page=2")
	p := apipage.Page{Current: 2, PerPage: 30, Total: 120}
	h := p.LinkHeader("https://shithub.sh", u)

	links := parseLink(h)
	wantRels := []string{"first", "prev", "next", "last"}
	for _, rel := range wantRels {
		if _, ok := links[rel]; !ok {
			t.Errorf("missing rel=%q in %s", rel, h)
		}
	}
	if got := pageFor(links["next"]); got != "3" {
		t.Errorf("next page: got %s, want 3", got)
	}
	if got := pageFor(links["prev"]); got != "1" {
		t.Errorf("prev page: got %s, want 1", got)
	}
	if got := pageFor(links["last"]); got != "4" {
		t.Errorf("last page: got %s, want 4", got)
	}
	if got := pageFor(links["first"]); got != "1" {
		t.Errorf("first page: got %s, want 1", got)
	}
	if !strings.HasPrefix(links["next"], "https://shithub.sh/api/v1/user/starred?") {
		t.Errorf("next link not absolute: %s", links["next"])
	}
}

func TestLinkHeader_FirstPage(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/repos?per_page=10&page=1")
	p := apipage.Page{Current: 1, PerPage: 10, Total: 25}
	links := parseLink(p.LinkHeader("https://shithub.sh", u))
	if _, ok := links["prev"]; ok {
		t.Errorf("prev should be absent on first page; got %v", links)
	}
	if _, ok := links["next"]; !ok {
		t.Error("next should be present on first page")
	}
	if got := pageFor(links["last"]); got != "3" {
		t.Errorf("last page: got %s, want 3", got)
	}
}

func TestLinkHeader_LastPage(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/repos?per_page=10&page=3")
	p := apipage.Page{Current: 3, PerPage: 10, Total: 25}
	links := parseLink(p.LinkHeader("https://shithub.sh", u))
	if _, ok := links["next"]; ok {
		t.Errorf("next should be absent on last page; got %v", links)
	}
	if _, ok := links["prev"]; !ok {
		t.Error("prev should be present on last page")
	}
}

func TestLinkHeader_SinglePage(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/repos?per_page=30&page=1")
	p := apipage.Page{Current: 1, PerPage: 30, Total: 5}
	if got := p.LinkHeader("https://shithub.sh", u); got != "" {
		t.Errorf("expected empty header for single-page result; got %q", got)
	}
}

func TestLinkHeader_StreamForm(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/feed?page=2")
	p := apipage.Page{Current: 2, PerPage: 30, Total: -1, HasMore: true}
	links := parseLink(p.LinkHeader("https://shithub.sh", u))
	if _, ok := links["first"]; ok {
		t.Error("first should not appear when total unknown")
	}
	if _, ok := links["last"]; ok {
		t.Error("last should not appear when total unknown")
	}
	if _, ok := links["next"]; !ok {
		t.Error("next should appear when HasMore=true")
	}
	if _, ok := links["prev"]; !ok {
		t.Error("prev should appear when Current > 1")
	}
}

func TestLinkHeader_StreamFormExhausted(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/feed?page=4")
	p := apipage.Page{Current: 4, PerPage: 30, Total: -1, HasMore: false}
	links := parseLink(p.LinkHeader("https://shithub.sh", u))
	if _, ok := links["next"]; ok {
		t.Error("next should be absent when HasMore=false")
	}
	if _, ok := links["prev"]; !ok {
		t.Error("prev should still appear when Current > 1")
	}
}

func TestLinkHeader_PreservesOtherQueryParams(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/issues?state=open&labels=bug,ux&per_page=30&page=2")
	p := apipage.Page{Current: 2, PerPage: 30, Total: 120}
	links := parseLink(p.LinkHeader("https://shithub.sh", u))
	for rel, link := range links {
		if !strings.Contains(link, "state=open") {
			t.Errorf("rel=%s lost state=open: %s", rel, link)
		}
		if !strings.Contains(link, "labels=") {
			t.Errorf("rel=%s lost labels: %s", rel, link)
		}
	}
}

func TestLinkHeader_RelativeWhenBaseURLEmpty(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/api/v1/repos?page=1&per_page=10")
	p := apipage.Page{Current: 1, PerPage: 10, Total: 25}
	h := p.LinkHeader("", u)
	if !strings.HasPrefix(h, "</api/v1/repos?") {
		t.Errorf("expected path-relative link; got %s", h)
	}
}

// parseLink reimplements the gh-compatible parser used by
// shithub-cli/internal/api.ParseLinkHeader. Kept in-test so apipage has
// no cross-module dependency, but we exercise the same algorithm here.
func parseLink(header string) map[string]string {
	out := map[string]string{}
	if header == "" {
		return out
	}
	for _, entry := range splitLinkEntries(header) {
		u, rel, ok := parseLinkEntry(entry)
		if !ok {
			continue
		}
		out[rel] = u
	}
	return out
}

func splitLinkEntries(header string) []string {
	var (
		entries []string
		buf     strings.Builder
		depth   int
	)
	for _, r := range header {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				entries = append(entries, strings.TrimSpace(buf.String()))
				buf.Reset()
				continue
			}
		}
		buf.WriteRune(r)
	}
	if buf.Len() > 0 {
		entries = append(entries, strings.TrimSpace(buf.String()))
	}
	return entries
}

func parseLinkEntry(entry string) (linkURL, rel string, ok bool) {
	lt := strings.Index(entry, "<")
	gt := strings.Index(entry, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return "", "", false
	}
	linkURL = entry[lt+1 : gt]
	rest := entry[gt+1:]
	for _, attr := range strings.Split(rest, ";") {
		attr = strings.TrimSpace(attr)
		if !strings.HasPrefix(attr, "rel=") {
			continue
		}
		rel = strings.Trim(attr[len("rel="):], `"`)
	}
	if rel == "" {
		return "", "", false
	}
	return linkURL, rel, true
}

func pageFor(link string) string {
	idx := strings.Index(link, "?")
	if idx < 0 {
		return ""
	}
	q, err := url.ParseQuery(link[idx+1:])
	if err != nil {
		return ""
	}
	return q.Get("page")
}
