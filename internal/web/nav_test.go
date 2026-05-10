// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestNavRendersContextualRepoAndOrgHeaders(t *testing.T) {
	t.Parallel()
	navFS := fstest.MapFS{
		"_layout.html": {Data: []byte(`{{ define "layout" }}{{ template "nav" . }}{{ template "page" . }}{{ end }}`)},
		"page.html":    {Data: []byte(`{{ define "page" }}page{{ end }}`)},
	}
	for _, name := range []string{"_nav.html", "_nav_offcanvas.html", "_repo_subnav.html", "_org_subnav.html"} {
		body, err := fs.ReadFile(TemplatesFS(), name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		navFS[name] = &fstest.MapFile{Data: body}
	}
	r, err := render.New(navFS, render.Options{
		Octicons: func(name string) (template.HTML, bool) {
			return template.HTML(`<svg data-icon="` + name + `"></svg>`), true //nolint:gosec // test-only constant markup
		},
	})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	cases := []struct {
		name    string
		data    map[string]any
		want    []string
		wantNot []string
	}{
		{
			name: "repo",
			data: map[string]any{
				"Viewer":       map[string]any{"ID": int64(1), "Username": "mfwolffe"},
				"CSRFToken":    "token",
				"Owner":        "tenseleyFlow",
				"Repo":         map[string]any{"Name": "shithub", "Visibility": "public"},
				"RepoCounts":   map[string]any{"HasIssues": true, "Issues": 1, "Pulls": 2, "Forks": 0},
				"ActiveSubnav": "code",
			},
			want: []string{
				`class="shithub-nav has-context"`,
				`data-offcanvas-open`,
				`role="dialog" aria-modal="true" aria-label="Global navigation"`,
				`aria-label="Repository"`,
				`href="/tenseleyFlow/shithub" class="is-strong">shithub</a>`,
				`href="/tenseleyFlow/shithub" class="shithub-offcanvas-repo-item"`,
				`href="/tenseleyFlow/shithub/issues"`,
				`Pull requests`,
			},
			wantNot: []string{`class="shithub-nav-links"`, `href="/about"`, `aria-label="Organization"`, `Copilot`},
		},
		{
			name: "org",
			data: map[string]any{
				"Viewer":       map[string]any{"ID": int64(1), "Username": "mfwolffe"},
				"CSRFToken":    "token",
				"Org":          map[string]any{"Slug": "tenseleyFlow"},
				"RepoCount":    37,
				"MemberCount":  2,
				"IsOwner":      true,
				"ActiveOrgNav": "teams",
			},
			want: []string{
				`class="shithub-nav has-context"`,
				`data-offcanvas-open`,
				`role="dialog" aria-modal="true" aria-label="Global navigation"`,
				`aria-label="Organization"`,
				`href="/tenseleyFlow" class="is-strong">tenseleyFlow</a>`,
				`href="/tenseleyFlow#org-repositories" class="shithub-offcanvas-repo-item"`,
				`href="/tenseleyFlow/teams"`,
				`href="/tenseleyFlow/people"`,
			},
			wantNot: []string{`class="shithub-nav-links"`, `href="/about"`, `Copilot`},
		},
		{
			name: "global",
			data: map[string]any{
				"Viewer":    map[string]any{"ID": int64(1), "Username": "mfwolffe"},
				"CSRFToken": "token",
			},
			want: []string{
				`class="shithub-nav"`,
				`data-offcanvas-open`,
				`role="dialog" aria-modal="true" aria-label="Global navigation"`,
				`All pull requests`,
				`href="/explore"`,
				`href="/about"`,
			},
			wantNot: []string{`shithub-nav-local`, `aria-label="Repository"`, `aria-label="Organization"`, `Copilot`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := r.Render(&buf, "page", tc.data); err != nil {
				t.Fatalf("Render: %v", err)
			}
			html := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(html, want) {
					t.Errorf("rendered nav missing %q in:\n%s", want, html)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(html, unwanted) {
					t.Errorf("rendered nav unexpectedly contained %q in:\n%s", unwanted, html)
				}
			}
		})
	}
}
