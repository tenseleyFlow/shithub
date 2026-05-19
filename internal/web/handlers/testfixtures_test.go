// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// testTemplatesFS returns a minimal in-memory templates filesystem matching
// the layout the production embed produces (after Sub-rooting).
func testTemplatesFS(t *testing.T) fs.FS {
	t.Helper()
	return fstest.MapFS{
		"_layout.html": &fstest.MapFile{
			Data: []byte(`{{ define "layout" }}<!DOCTYPE html><html><head>{{ if stringField . "MetaDescription" }}<meta name="description" content="{{ stringField . "MetaDescription" }}">{{ end }}{{ with stringField . "CanonicalURL" }}<link rel="canonical" href="{{ . }}">{{ end }}<title>{{ .Title }} · shithub</title></head><body>{{ template "page" . }}</body></html>{{ end }}`),
		},
		"hello.html": &fstest.MapFile{
			Data: []byte(`{{ define "page" }}<main>shithub - GitHub. Open source. Without Copilot. Sprint 00 v{{ .Version }}</main>{{ end }}`),
		},
		"about.html": &fstest.MapFile{
			Data: []byte(`{{ define "page" }}<main>No hard feelings to GitHub. I just don't want AI training on my code.</main>{{ end }}`),
		},
		"codespaces.html": &fstest.MapFile{
			Data: []byte(`{{ define "page" }}<main>Hosted development environments are not implemented yet. Actions runners execute short-lived CI jobs. Codespaces remain a paid-organization launch blocker.</main>{{ end }}`),
		},
	}
}

// testStaticFS returns a minimal in-memory static filesystem matching the
// layout the production embed produces.
func testStaticFS(t *testing.T) fs.FS {
	t.Helper()
	return fstest.MapFS{
		"logo/shithub.svg": &fstest.MapFile{
			Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`),
		},
		"css/shithub.css": &fstest.MapFile{
			Data: []byte(`body { color: black; }`),
		},
	}
}
