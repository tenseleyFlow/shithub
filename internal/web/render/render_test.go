// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Each test builds a tiny in-memory template tree to exercise one
// invariant of New(). Keep these focused; broader render flows are
// covered by handler-level tests.

func TestNew_RegistersRootPages(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(`{{ define "layout" }}<html>{{ template "body" . }}</html>{{ end }}`)},
		"home.html":    &fstest.MapFile{Data: []byte(`{{ define "body" }}home page{{ end }}`)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	if err := r.Render(&buf, "home", nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "home page") {
		t.Errorf("rendered output missing body: %q", buf.String())
	}
}

func TestNew_RegistersSubdirPages(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html":    &fstest.MapFile{Data: []byte(`{{ define "layout" }}<html>{{ template "body" . }}</html>{{ end }}`)},
		"errors/404.html": &fstest.MapFile{Data: []byte(`{{ define "body" }}not found{{ end }}`)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	if err := r.Render(&buf, "errors/404", nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("rendered output missing body: %q", buf.String())
	}
}

func TestRenderFragmentExecutesPageWithoutLayout(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(
			`{{ define "layout" }}<html>{{ template "page" . }}</html>{{ end }}`,
		)},
		"fragment.html": &fstest.MapFile{Data: []byte(
			`{{ define "page" }}fragment only{{ end }}`,
		)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	if err := r.RenderFragment(&buf, "fragment", nil); err != nil {
		t.Fatalf("render fragment: %v", err)
	}
	if got := buf.String(); got != "fragment only" {
		t.Fatalf("RenderFragment body = %q, want fragment only", got)
	}
}

// Regression test for the inbound deferral from S30 dogfood: a partial
// at `profile/_tabs.html` that defines `{{ define "tabs" }}` was
// silently registered as an unparsed page. A page that called
// `{{ template "tabs" . }}` then rendered blank.
func TestNew_LoadsSubdirPartials(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(
			`{{ define "layout" }}<html>{{ template "body" . }}</html>{{ end }}`,
		)},
		"profile/_tabs.html": &fstest.MapFile{Data: []byte(
			`{{ define "tabs" }}TAB CONTENT{{ end }}`,
		)},
		"profile.html": &fstest.MapFile{Data: []byte(
			`{{ define "body" }}{{ template "tabs" . }}{{ end }}`,
		)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	if err := r.Render(&buf, "profile", nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "TAB CONTENT") {
		t.Errorf("subdir partial not loaded — body was %q", buf.String())
	}
}

// A page that references a template name nothing defines should fail
// LOUDLY at New() time, not silently render blank at exec time.
func TestNew_FailsOnUndefinedTemplateRef(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(
			`{{ define "layout" }}<html>{{ template "body" . }}</html>{{ end }}`,
		)},
		"broken.html": &fstest.MapFile{Data: []byte(
			`{{ define "body" }}{{ template "does-not-exist" . }}{{ end }}`,
		)},
	}
	_, err := New(fsys, Options{})
	if err == nil {
		t.Fatal("New: expected error for undefined template ref, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing template; got %v", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error should name the offending page; got %v", err)
	}
}

// Sanity: refs into partials still resolve (not flagged as undefined).
func TestNew_AcceptsRefsResolvedByPartials(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(
			`{{ define "layout" }}{{ template "header" . }}{{ template "body" . }}{{ end }}`,
		)},
		"_header.html": &fstest.MapFile{Data: []byte(
			`{{ define "header" }}HDR{{ end }}`,
		)},
		"page.html": &fstest.MapFile{Data: []byte(
			`{{ define "body" }}body{{ end }}`,
		)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	if err := r.Render(&buf, "page", nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "HDR") || !strings.Contains(got, "body") {
		t.Errorf("missing partial or page output: %q", got)
	}
}

// RenderPage preserves any Viewer / CSRFToken the handler set itself,
// rather than overwriting them. The auto-inject is for handlers that
// hand RenderPage an empty map; explicit handlers stay in control.
func TestRenderPage_PreservesExplicitViewer(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"_layout.html": &fstest.MapFile{Data: []byte(
			`{{ define "layout" }}{{ template "body" . }}{{ end }}`,
		)},
		"page.html": &fstest.MapFile{Data: []byte(
			`{{ define "body" }}viewer={{ .Viewer }}{{ end }}`,
		)},
	}
	r, err := New(fsys, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	if err := r.RenderPage(rw, req, "page", map[string]any{
		"Viewer": "explicit",
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rw.Body.String(), "viewer=explicit") {
		t.Errorf("explicit Viewer was overwritten; body=%q", rw.Body.String())
	}
}
