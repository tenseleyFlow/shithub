// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// rendererHeapCeilingBytes bounds the live heap a booted shithubd-web
// may spend on parsed templates.
//
// Why this test exists: until 2026-09 the web wiring built one
// *render.Renderer per handler set — eight of them — and each parses
// every page template with its reachable partials cloned in. Eight
// renderers measured 664 MB of *static* live heap on a 3.9 GB box,
// which is what OOM-killed shithubd (docs/internal/retro/
// 2026-09-02-availability-sitrep.md). One shared renderer with pruned
// partials measures ~41 MB.
//
// The ceiling is deliberately ~3.5x the measured value: it is a
// regression tripwire for "someone reintroduced per-handler-set
// renderers" or "the template set tripled", not a byte budget, and it
// must not flake on a noisy CI runner.
const rendererHeapCeilingBytes = 150 << 20

// TestSharedRendererHeapCeiling measures the live heap of the renderer
// set a production boot constructs — which, post-2026-09, is exactly
// one — and fails if it exceeds the ceiling.
func TestSharedRendererHeapCeiling(t *testing.T) {
	// Not parallel: it reads process-wide heap statistics.
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	before := m.HeapAlloc

	rr, err := render.New(TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&m)
	used := m.HeapAlloc - before
	// Keep rr reachable across the measurement so the GC cannot
	// collect the very thing being measured.
	runtime.KeepAlive(rr)

	t.Logf("shared renderer live heap: %.1f MB (ceiling %.0f MB)",
		float64(used)/(1<<20), float64(rendererHeapCeilingBytes)/(1<<20))
	if used > rendererHeapCeilingBytes {
		t.Fatalf("renderer heap %.1f MB exceeds ceiling %.0f MB; did the wiring go back to one renderer per handler set, or did a partial get pulled into every page?",
			float64(used)/(1<<20), float64(rendererHeapCeilingBytes)/(1<<20))
	}
}

// TestWebWiringBuildsExactlyOneRenderer is the structural half of the
// invariant. The heap ceiling above would catch a regression only if
// the extra renderers were built in one process during one test; this
// catches a new `render.New` in the wiring the moment it is written.
//
// Handler *tests* may still call render.New freely — this only scans
// non-test sources in package web.
func TestWebWiringBuildsExactlyOneRenderer(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "render" {
				return true
			}
			found = append(found, fset.Position(call.Pos()).String())
			return true
		})
	}
	if len(found) != 1 {
		t.Fatalf("package web must construct exactly one render.Renderer; found %d: %v\nhint: handler builders take the shared *render.Renderer built in server.go", len(found), found)
	}
	if !strings.HasPrefix(found[0], "server.go:") {
		t.Errorf("the single render.New should live in server.go; found at %s", found[0])
	}
}

// BenchmarkRendererNew reports the per-renderer construction cost and
// allocation volume. `go test -bench RendererNew -benchmem ./internal/web`
// is the quickest way to see the effect of a template-loader change.
func BenchmarkRendererNew(b *testing.B) {
	fsys := TemplatesFS()
	opts := render.Options{Octicons: render.BuiltinOcticons()}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rr, err := render.New(fsys, opts)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(rr)
	}
}
