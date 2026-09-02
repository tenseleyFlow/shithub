// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"bytes"
	"html/template"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
)

// newAllPartials is the pre-2026-09 loader: it parses *every* partial
// into *every* page. It exists only so TestPartialPruning_OutputParity
// can prove the pruned loader in New() produces byte-identical output.
// If New() ever changes shape, mirror the change here.
func newAllPartials(tmplFS fs.FS, opts Options) (*Renderer, error) {
	var partialPaths, pagePaths []string
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
		return nil, err
	}
	sort.Strings(partialPaths)
	sort.Strings(pagePaths)

	r := &Renderer{pages: map[string]*template.Template{}, octicon: opts.Octicons}
	for _, page := range pagePaths {
		t := template.New(path.Base(page)).Funcs(funcMap(r.octicon))
		all := append(append([]string{}, partialPaths...), page)
		parsed, err := t.ParseFS(tmplFS, all...)
		if err != nil {
			return nil, err
		}
		r.pages[strings.TrimSuffix(page, ".html")] = parsed
	}
	return r, nil
}

// productionTemplates is the real template tree the web binary embeds.
// Read from disk rather than via web.TemplatesFS() to avoid an import
// cycle (package web imports package render).
func productionTemplates(t *testing.T) fs.FS {
	t.Helper()
	fsys := os.DirFS("../templates")
	if _, err := fs.Stat(fsys, "_layout.html"); err != nil {
		t.Fatalf("template tree not found at ../templates: %v", err)
	}
	return fsys
}

// TestPartialPruning_OutputParity renders every production page through
// both loaders with identical data and requires identical results —
// same bytes on success, same error text on failure. This is the safety
// net for New()'s reachability pruning: if the closure ever drops a
// partial a page actually reaches, a page's output changes here.
func TestPartialPruning_OutputParity(t *testing.T) {
	t.Parallel()
	fsys := productionTemplates(t)
	opts := Options{Octicons: BuiltinOcticons()}

	pruned, err := New(fsys, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	full, err := newAllPartials(fsys, opts)
	if err != nil {
		t.Fatalf("newAllPartials: %v", err)
	}
	if len(pruned.pages) != len(full.pages) {
		t.Fatalf("page count: pruned=%d full=%d", len(pruned.pages), len(full.pages))
	}

	names := make([]string, 0, len(pruned.pages))
	for name := range pruned.pages {
		names = append(names, name)
	}
	sort.Strings(names)

	var rendered int
	for _, name := range names {
		data := map[string]any{"Title": "parity", "Message": "parity", "Status": 404}
		var a, b bytes.Buffer
		errA := pruned.Render(&a, name, data)
		errB := full.Render(&b, name, data)
		switch {
		case (errA == nil) != (errB == nil):
			t.Errorf("%s: pruned err=%v full err=%v", name, errA, errB)
		case errA != nil && errA.Error() != errB.Error():
			t.Errorf("%s: error text diverged:\n pruned: %v\n full:   %v", name, errA, errB)
		case errA == nil:
			rendered++
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Errorf("%s: rendered output differs (pruned %d bytes, full %d bytes)",
					name, a.Len(), b.Len())
			}
		}
	}
	// Guard against the test silently degrading into "every page errored
	// identically" and proving nothing about output.
	if rendered < 20 {
		t.Fatalf("only %d/%d pages rendered successfully; parity check is not meaningful", rendered, len(names))
	}
	t.Logf("compared %d pages, %d rendered clean", len(names), rendered)
}
