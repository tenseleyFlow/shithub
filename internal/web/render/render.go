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
	"reflect"
	"sort"
	"strings"
	"text/template/parse"
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

// New parses every page template under tmplFS.
//
// Naming contract — read this before adding files to internal/web/templates/:
//
//   - **Pages** are .html files whose basename does NOT begin with an
//     underscore. A page at `repo/tree.html` is registered under the
//     lookup name `repo/tree`. Render that name from a handler.
//   - **Partials** are .html files whose basename begins with an
//     underscore (`_layout.html`, `profile/_tabs.html`). Partials are
//     parsed into *every* page so the `{{ define "name" }}` blocks
//     they declare are resolvable from any page template.
//
// Both pages and partials are picked up recursively. Earlier versions
// of this loader walked only the root for partials, which caused a
// page that referenced a subdir partial (`profile/_tabs.html`'s
// `{{ define "tabs" }}`) to render blank — html/template silently
// ignored the missing-template ref at exec time. We now also validate
// that every `{{ template "name" }}` action in every parsed page
// resolves; an undefined ref fails loud at startup with the offending
// page + the missing name.
func New(tmplFS fs.FS, opts Options) (*Renderer, error) {
	var (
		partialPaths []string
		pagePaths    []string
	)
	if err := fs.WalkDir(tmplFS, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		if strings.HasPrefix(path.Base(p), "_") {
			partialPaths = append(partialPaths, p)
		} else {
			pagePaths = append(pagePaths, p)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk templates: %w", err)
	}
	sort.Strings(partialPaths)
	sort.Strings(pagePaths)

	r := &Renderer{
		pages:   make(map[string]*template.Template, len(pagePaths)),
		octicon: opts.Octicons,
	}

	parsePage := func(displayName, primary string) error {
		t := template.New(path.Base(primary)).Funcs(funcMap(r.octicon))
		all := append([]string{}, partialPaths...)
		all = append(all, primary)
		parsed, err := t.ParseFS(tmplFS, all...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", displayName, err)
		}
		if missing := undefinedTemplateRefs(parsed); len(missing) > 0 {
			return fmt.Errorf("page %q references undefined template(s): %s", displayName, strings.Join(missing, ", "))
		}
		r.pages[displayName] = parsed
		return nil
	}

	for _, page := range pagePaths {
		if err := parsePage(strings.TrimSuffix(page, ".html"), page); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// undefinedTemplateRefs returns the names of every `{{ template "name" }}`
// action in any parsed sub-template that does not resolve to a defined
// template within `t`. Empty slice means every reference is satisfied.
//
// The standard library does not validate this at parse time — html/template
// happily parses a page with a dangling `{{ template "missing" }}` and
// silently emits nothing at exec time. This helper closes that hole.
func undefinedTemplateRefs(t *template.Template) []string {
	defined := map[string]bool{}
	for _, child := range t.Templates() {
		defined[child.Name()] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, child := range t.Templates() {
		if child.Tree == nil {
			continue
		}
		walkTemplateRefs(child.Tree.Root, func(name string) {
			if defined[name] || seen[name] {
				return
			}
			seen[name] = true
			missing = append(missing, name)
		})
	}
	sort.Strings(missing)
	return missing
}

func walkTemplateRefs(n parse.Node, visit func(name string)) {
	if n == nil {
		return
	}
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			walkTemplateRefs(c, visit)
		}
	case *parse.IfNode:
		walkTemplateRefs(x.List, visit)
		walkTemplateRefs(x.ElseList, visit)
	case *parse.RangeNode:
		walkTemplateRefs(x.List, visit)
		walkTemplateRefs(x.ElseList, visit)
	case *parse.WithNode:
		walkTemplateRefs(x.List, visit)
		walkTemplateRefs(x.ElseList, visit)
	case *parse.TemplateNode:
		visit(x.Name)
	}
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

// RenderFragment executes only a page template's "page" definition. Use it
// for HTML fragments returned to JavaScript or htmx; full browser pages should
// continue to call Render/RenderPage so nav, footer, and document chrome stay
// consistent.
func (r *Renderer) RenderFragment(w io.Writer, name string, data any) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("render: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "page", data); err != nil {
		return fmt.Errorf("execute fragment %s: %w", name, err)
	}
	if rw, ok := w.(http.ResponseWriter); ok {
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
		// flag reads an optional boolean-ish field from map or struct
		// template data. Layout-level feature toggles use this so pages
		// backed by typed structs don't fail when the toggle is absent.
		"flag": dataFlag,
		// stringField reads an optional string-ish field from map or
		// struct template data. Shared document metadata uses this so
		// typed page-data structs don't all have to define every optional
		// SEO/social field.
		"stringField": dataString,
		// jsField returns only server-marked template.JS values. It is
		// intentionally stricter than stringField so JSON-LD can be
		// emitted from trusted marshaled data without giving templates a
		// generic raw-JS escape hatch for arbitrary strings.
		"jsField": dataJS,
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
		// percent formats a 0.0–1.0 ratio as a whole-percent string
		// (e.g. 0.97 → "97%"). Status page uses this; reusable for
		// any future "X out of 100" display.
		"percent": func(v float64) string {
			return fmt.Sprintf("%.0f%%", v*100)
		},
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
		// verifiedReasonMessage maps a sigverify.Reason string to a
		// short user-facing explanation. Used by the _verified_badge
		// partial's popover. Accepts the Reason as a string so the
		// template doesn't need a type assertion.
		"verifiedReasonMessage": verifiedReasonMessage,
	}
}

// verifiedReasonMessage returns the popover body text for each
// non-valid verification reason. "unsigned" never reaches the
// popover (the badge isn't rendered at all in that case) so it's
// omitted. The strings are user-facing copy; keep them short and
// concrete.
func verifiedReasonMessage(reason any) string {
	r, ok := reason.(string)
	if !ok {
		// The template might pass a typed sigverify.Reason; Stringer
		// would surface the underlying string. Try fmt-style fallback.
		r = fmt.Sprintf("%s", reason)
	}
	switch r {
	case "unknown_key":
		return "This commit was signed but the public key isn't registered with shithub."
	case "bad_email":
		return "The commit signature doesn't match any of the registered user's emails."
	case "unverified_email":
		return "The commit signature matches an email the user hasn't verified."
	case "malformed_signature":
		return "The commit's signature couldn't be parsed."
	case "invalid":
		return "The commit's signature failed cryptographic verification."
	case "expired_key":
		return "The signing key was expired when this commit was made."
	case "not_signing_key":
		return "The key that signed this commit isn't authorized for signing."
	default:
		return "This commit's signature couldn't be verified."
	}
}

func dataFlag(data any, name string) bool {
	field, ok := dataField(data, name)
	if !ok {
		return false
	}
	return truthyValue(field)
}

func dataString(data any, name string) string {
	field, ok := dataField(data, name)
	if !ok {
		return ""
	}
	for field.Kind() == reflect.Pointer || field.Kind() == reflect.Interface {
		if field.IsNil() {
			return ""
		}
		field = field.Elem()
	}
	switch field.Kind() {
	case reflect.String:
		return field.String()
	default:
		if field.CanInterface() {
			return fmt.Sprint(field.Interface())
		}
		return ""
	}
}

func dataJS(data any, name string) template.JS {
	field, ok := dataField(data, name)
	if !ok {
		return ""
	}
	if field.CanInterface() {
		switch v := field.Interface().(type) {
		case template.JS:
			return v
		case *template.JS:
			if v != nil {
				return *v
			}
		}
	}
	for field.Kind() == reflect.Pointer || field.Kind() == reflect.Interface {
		if field.IsNil() {
			return ""
		}
		field = field.Elem()
		if field.CanInterface() {
			if v, ok := field.Interface().(template.JS); ok {
				return v
			}
		}
	}
	return ""
}

func dataField(data any, name string) (reflect.Value, bool) {
	v := reflect.ValueOf(data)
	if !v.IsValid() {
		return reflect.Value{}, false
	}
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, false
		}
		field := v.MapIndex(reflect.ValueOf(name))
		if !field.IsValid() {
			return reflect.Value{}, false
		}
		return field, true
	case reflect.Struct:
		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanInterface() {
			return reflect.Value{}, false
		}
		return field, true
	default:
		return reflect.Value{}, false
	}
}

func truthyValue(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.String:
		return v.String() != ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() != 0
	default:
		return !v.IsZero()
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
