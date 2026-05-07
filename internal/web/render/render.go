// SPDX-License-Identifier: AGPL-3.0-or-later

// Package render owns the html/template loading and rendering pipeline.
// S02 will broaden this with helpers (relativeTime, urlFor, octicon, etc.);
// S00 ships only what the hello page needs.
package render

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path"
	"strings"
)

// Renderer holds parsed templates indexed by page name.
type Renderer struct {
	pages map[string]*template.Template
}

// New parses every page template under tmplFS. A "page template" is any file
// at the root of tmplFS that does NOT begin with an underscore. Files that
// begin with an underscore (e.g. "_layout.html") are partials, parsed once
// into every page.
func New(tmplFS fs.FS) (*Renderer, error) {
	entries, err := fs.ReadDir(tmplFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read template root: %w", err)
	}

	var (
		partialNames []string
		pageNames    []string
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

	r := &Renderer{pages: make(map[string]*template.Template, len(pageNames))}
	for _, page := range pageNames {
		t := template.New(page).Funcs(funcMap())
		all := append([]string{}, partialNames...)
		all = append(all, page)
		// Convert to filesystem paths.
		for i := range all {
			all[i] = path.Clean(all[i])
		}
		parsed, err := t.ParseFS(tmplFS, all...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		r.pages[strings.TrimSuffix(page, ".html")] = parsed
	}
	return r, nil
}

// Render writes the named page to w using data as the template root context.
// The page's templates execute the layout via {{ template "layout" . }}.
func (r *Renderer) Render(w io.Writer, name string, data any) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("render: unknown page %q", name)
	}
	// Pages declare a "page" block; the layout calls into it.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("execute %s: %w", name, err)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		// safeHTML embeds trusted HTML directly. Callers MUST ensure the
		// input is server-controlled — never user input. S25's markdown
		// pipeline supplies the canonical helper for user content.
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s) // #nosec G203 — trusted-input only
		},
	}
}
