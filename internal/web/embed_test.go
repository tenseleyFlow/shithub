// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io"
	"io/fs"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// TestProductionTemplatesParse runs the embedded production templates
// through render.New so any undefined-template ref or unparseable file
// fails CI rather than the binary's first request after deploy.
//
// The check exists in addition to the targeted unit tests in
// internal/web/render/render_test.go: those cover the validator's
// behaviour on synthetic FS trees; this one covers the actual files
// under internal/web/templates/.
func TestProductionTemplatesParse(t *testing.T) {
	t.Parallel()
	_, err := render.New(TemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("production templates failed to parse: %v", err)
	}
}

// TestProductionTemplatesPartialsTolerateEmptyData renders every page
// in the production tree with an empty map[string]any and fails the
// test only if an error originates in a shared partial (filename
// starting with `_`, e.g. _nav.html / _layout.html).
//
// Why this matters: html/template parses fine even when a partial
// references a field nothing populates. The error only fires at
// execute time. A live site went 500 in May 2026 when _nav.html
// started referencing .GlobalSearchQuery and the homepage's
// helloData struct didn't carry it.
//
// We pass an empty map (not a typed struct) because:
//  1. map[string]any tolerates missing keys (`with .X` evaluates nil).
//  2. So any error that *does* fire from inside _nav.html or
//     _layout.html is necessarily a real defect: a partial referencing
//     a field with semantics that can't be nil (e.g. ranging over a
//     non-map type, or calling a method on a nil interface).
//
// Page-internal errors (from non-partial templates) are expected with
// empty data and are filtered out — those are exercised by the
// handler-level tests that pass realistic fixtures. For the typed-
// struct regression specifically, see the hello handler test in
// internal/web/handlers/.
func TestProductionTemplatesPartialsTolerateEmptyData(t *testing.T) {
	t.Parallel()
	tmplFS := TemplatesFS()
	r, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	pages, err := listPages(tmplFS)
	if err != nil {
		t.Fatalf("listPages: %v", err)
	}
	if len(pages) == 0 {
		t.Fatal("no pages discovered under templates/ — fixture broken")
	}
	req := httptest.NewRequest("GET", "/", nil)
	for _, name := range pages {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rw := httptest.NewRecorder()
			err := r.RenderPage(rw, req, name, map[string]any{})
			if err != nil && errorOriginatesInPartial(err) {
				t.Errorf("page %q: shared partial errored on empty data: %v", name, err)
			}
			_, _ = io.Copy(io.Discard, rw.Body)
		})
	}
}

func TestOrgPagesRenderSingleSharedOrgNav(t *testing.T) {
	t.Parallel()
	r, err := render.New(TemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	data := map[string]any{
		"Title":         "Organization",
		"Org":           map[string]any{"Slug": "gardesk", "DisplayName": "gardesk"},
		"AvatarURL":     "/avatars/gardesk",
		"ActiveOrgNav":  "repositories",
		"RepoCount":     5,
		"FilteredCount": 5,
		"PageCount":     1,
		"MemberCount":   1,
		"IsOwner":       true,
		"Locked":        true,
		"UpgradeBanner": map[string]any{
			"Message":    "Security overview features require Team billing.",
			"ActionText": "Manage billing and plans",
			"ActionHref": "/organizations/gardesk/settings/billing",
		},
		"Form": map[string]any{
			"DisplayName":           "gardesk",
			"Description":           "",
			"Website":               "",
			"Location":              "",
			"BillingEmail":          "",
			"AllowMemberRepoCreate": true,
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	for _, page := range []string{"orgs/repositories", "orgs/security", "orgs/settings_profile"} {
		t.Run(page, func(t *testing.T) {
			t.Parallel()
			rw := httptest.NewRecorder()
			if err := r.RenderPage(rw, req, page, data); err != nil {
				t.Fatalf("RenderPage: %v", err)
			}
			if got := strings.Count(rw.Body.String(), `<nav class="shithub-org-nav"`); got != 1 {
				t.Fatalf("org nav count = %d, want 1", got)
			}
		})
	}
}

func TestExploreFeedFragmentAppendsRowsAndReplacesPagination(t *testing.T) {
	t.Parallel()
	r, err := render.New(TemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	rw := httptest.NewRecorder()
	if err := r.RenderFragment(rw, "explore/feed_page", map[string]any{
		"FeedHasNext": true,
		"FeedNextURL": "/explore?before=2026-05-12T03%3A00%3A00Z~42",
	}); err != nil {
		t.Fatalf("RenderFragment: %v", err)
	}
	body := rw.Body.String()
	for _, want := range []string{
		`id="shithub-feed-fragment-rows"`,
		`hx-target="#shithub-feed-list"`,
		`hx-swap="beforeend"`,
		`hx-select="#shithub-feed-fragment-rows > *"`,
		`hx-select-oob="#shithub-feed-pagination:outerHTML"`,
		`Loading...`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment missing %q in:\n%s", want, body)
		}
	}
}

// errorOriginatesInPartial returns true when an html/template execute
// error blames a file whose basename starts with `_`. Errors from such
// files are bugs in the partial because we render with an empty map
// (which should never cause field-existence failures).
func errorOriginatesInPartial(err error) bool {
	// Format: `template: file.html:LINE:COL: executing ...`
	s := err.Error()
	const prefix = "template: "
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			return false
		}
		s = s[i+len(prefix):]
		end := strings.IndexByte(s, ':')
		if end < 0 {
			return false
		}
		filename := s[:end]
		if strings.HasPrefix(path.Base(filename), "_") {
			return true
		}
		s = s[end:]
	}
}

// listPages walks tmplFS and returns the lookup names render.New uses
// (path without trailing .html, partials excluded).
func listPages(tmplFS fs.FS) ([]string, error) {
	var pages []string
	err := fs.WalkDir(tmplFS, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		if strings.HasPrefix(path.Base(p), "_") {
			return nil
		}
		pages = append(pages, strings.TrimSuffix(p, ".html"))
		return nil
	})
	return pages, err
}
