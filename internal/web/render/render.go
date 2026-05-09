// SPDX-License-Identifier: AGPL-3.0-or-later

// Package render owns the html/template loading and rendering pipeline.
// S02 ships the helper set that the rest of the project will rely on
// (safeHTML, relativeTime, pluralize, pathJoin, octicon, csrfToken).
// S25 will broaden this with the markdown pipeline.
package render

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// Renderer holds parsed templates indexed by page name.
type Renderer struct {
	pages   map[string]*template.Template
	octicon OcticonResolver
}

// OcticonResolver returns the inline SVG markup for a named octicon. The
// implementation is provided by the caller; for S02 we ship a tiny built-in
// set; later sprints can plug in the full Primer octicon catalog.
type OcticonResolver func(name string) (template.HTML, bool)

// Options configures a renderer.
type Options struct {
	Octicons OcticonResolver
}

// New parses every page template under tmplFS. A "page template" is any file
// at the root of tmplFS that does NOT begin with an underscore. Files that
// begin with an underscore (e.g. "_layout.html") are partials, parsed once
// into every page.
func New(tmplFS fs.FS, opts Options) (*Renderer, error) {
	entries, err := fs.ReadDir(tmplFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read template root: %w", err)
	}

	var (
		partialNames []string
		pageNames    []string
		errorPages   []string
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		if strings.HasPrefix(name, "_") {
			partialNames = append(partialNames, name)
		} else {
			pageNames = append(pageNames, name)
		}
	}

	// Recursively pick up files in subdirectories like errors/.
	// Each subdirectory file is registered as `<dir>/<name>` (without
	// suffix) for Render lookups.
	if err := fs.WalkDir(tmplFS, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		if !strings.Contains(p, "/") {
			return nil
		}
		errorPages = append(errorPages, p)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk templates: %w", err)
	}

	r := &Renderer{
		pages:   make(map[string]*template.Template, len(pageNames)+len(errorPages)),
		octicon: opts.Octicons,
	}

	parse := func(displayName string, primary string) error {
		t := template.New(path.Base(primary)).Funcs(funcMap(r.octicon))
		all := append([]string{}, partialNames...)
		all = append(all, primary)
		parsed, err := t.ParseFS(tmplFS, all...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", displayName, err)
		}
		r.pages[displayName] = parsed
		return nil
	}

	for _, page := range pageNames {
		if err := parse(strings.TrimSuffix(page, ".html"), page); err != nil {
			return nil, err
		}
	}
	for _, page := range errorPages {
		if err := parse(strings.TrimSuffix(page, ".html"), page); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Render writes the named page to w using data as the template root context.
//
// Prefer RenderPage when a *http.Request is in scope — it auto-injects the
// viewer (current logged-in user) into map data so partials like _nav.html
// can branch on .Viewer without every handler remembering to thread it.
//
// When w is an http.ResponseWriter, Render sets Content-Type to
// `text/html; charset=utf-8` *before* the first body byte. This is
// load-bearing: a handler that calls WriteHeader(non-200) without
// pre-setting Content-Type otherwise produces a 4xx/5xx response with
// no Content-Type, which the browser renders as raw text. Setting it
// here makes that class of bug structurally impossible.
func (r *Renderer) Render(w io.Writer, name string, data any) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("render: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("execute %s: %w", name, err)
	}
	if rw, ok := w.(http.ResponseWriter); ok {
		// Header().Set is a no-op once headers have been committed
		// (e.g. an upstream WriteHeader call). That's the right
		// behaviour: we don't try to retroactively fix a header
		// stream that's already on the wire — the caller has to set
		// Content-Type before WriteHeader in those cases.
		if rw.Header().Get("Content-Type") == "" {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// RenderPage is the request-aware Render: when data is a map[string]any, it
// injects "Viewer" (from middleware.CurrentUserFromContext) and "CSRFToken"
// (the per-request token) if the caller hasn't set them. The nav partial's
// sign-out form uses the token, so every layout-rendered page needs it.
// Typed-struct callers must include those fields themselves — we don't
// reflect-mutate to avoid surprising aliasing.
func (r *Renderer) RenderPage(w io.Writer, req *http.Request, name string, data any) error {
	if m, ok := data.(map[string]any); ok {
		if _, present := m["Viewer"]; !present {
			m["Viewer"] = middleware.CurrentUserFromContext(req.Context())
		}
		if _, present := m["CSRFToken"]; !present {
			m["CSRFToken"] = middleware.CSRFTokenForRequest(req)
		}
		data = m
	}
	return r.Render(w, name, data)
}

// HTTPError writes an error page with the appropriate status code. If the
// named error template doesn't exist a plain-text fallback is written.
func (r *Renderer) HTTPError(w http.ResponseWriter, req *http.Request, status int, message string) {
	pageName := errorPageFor(status)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	data := struct {
		Title      string
		Status     int
		StatusText string
		Message    string
		RequestID  string
	}{
		Title:      fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Status:     status,
		StatusText: http.StatusText(status),
		Message:    message,
		RequestID:  middleware.RequestIDFromContext(req.Context()),
	}
	if err := r.Render(w, pageName, data); err != nil {
		_, _ = fmt.Fprintf(w, "%d %s\n%s\n(request_id=%s)\n",
			status, http.StatusText(status), message, data.RequestID)
	}
}

func errorPageFor(status int) string {
	switch status {
	case http.StatusForbidden:
		return "errors/403"
	case http.StatusNotFound:
		return "errors/404"
	case http.StatusTooManyRequests:
		return "errors/429"
	default:
		return "errors/500"
	}
}

func funcMap(octicon OcticonResolver) template.FuncMap {
	return template.FuncMap{
		// safeHTML embeds trusted HTML directly. Callers MUST ensure the
		// input is server-controlled — never user input. S25's markdown
		// pipeline supplies the canonical helper for user content.
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s) //nolint:gosec // trusted-input only
		},
		// relativeTime renders a "2 hours ago" / "yesterday" / "Mar 5"
		// style label. Used wherever timestamps appear in UI.
		"relativeTime": relativeTime,
		// pluralize picks the singular or plural form based on count.
		"pluralize": func(count int, one, many string) string {
			if count == 1 {
				return one
			}
			return many
		},
		// pathJoin builds URL paths with a single leading slash.
		"pathJoin": func(parts ...string) string {
			joined := path.Join(parts...)
			if !strings.HasPrefix(joined, "/") {
				return "/" + joined
			}
			return joined
		},
		// octicon resolves a named octicon to inline SVG. Returns empty
		// HTML if the icon isn't registered (the caller's template stays
		// valid but renders nothing — better than a build-time crash).
		"octicon": func(name string) template.HTML {
			if octicon == nil {
				return ""
			}
			if html, ok := octicon(name); ok {
				return html
			}
			return ""
		},
		// csrfToken pulls the per-request token from the request context.
		// Templates use this in <input type="hidden" name="csrf_token">.
		"csrfToken": middleware.CSRFTokenForRequest,
		// dict builds a map for partial-template includes that need
		// multiple named values (idiomatic Go template trick).
		// add / sub are tiny integer helpers used by pagination
		// templates (next/prev page links). Templates can't do
		// arithmetic, so the helpers earn their keep here.
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"dict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of args")
			}
			m := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: non-string key at %d", i)
				}
				m[key] = values[i+1]
			}
			return m, nil
		},
	}
}

// relativeTime returns a human-readable relative-time string. The intent is
// to read naturally; absolute precision below the level of "minutes" isn't
// useful for UI labels.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// Future timestamps are uncommon; render as absolute.
		return t.UTC().Format("Jan 2, 2006")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d / time.Minute)
		return fmt.Sprintf("%d minute%s ago", m, plural(m))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		return fmt.Sprintf("%d hour%s ago", h, plural(h))
	case d < 7*24*time.Hour:
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	case d < 30*24*time.Hour:
		w := int(d / (7 * 24 * time.Hour))
		return fmt.Sprintf("%d week%s ago", w, plural(w))
	case d < 365*24*time.Hour:
		mo := int(d / (30 * 24 * time.Hour))
		return fmt.Sprintf("%d month%s ago", mo, plural(mo))
	default:
		return t.UTC().Format("Jan 2, 2006")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
