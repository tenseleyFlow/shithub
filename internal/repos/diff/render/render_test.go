// SPDX-License-Identifier: AGPL-3.0-or-later

package render_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos/diff/parse"
	"github.com/tenseleyFlow/shithub/internal/repos/diff/render"
)

const samplePatch = `diff --git a/hello.txt b/hello.txt
index 1111111..2222222 100644
--- a/hello.txt
+++ b/hello.txt
@@ -1,3 +1,4 @@
 line one
-old second
+new second
+new third
 line three
`

func mustParse(t *testing.T, s string) *parse.Diff {
	t.Helper()
	d, err := parse.ParseBytes([]byte(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

func TestUnified_RendersAddDelContext(t *testing.T) {
	t.Parallel()
	out := render.Diff(mustParse(t, samplePatch), render.Options{Mode: render.ModeUnified})
	if !strings.Contains(out, "shithub-diff-unified") {
		t.Errorf("missing unified class")
	}
	if !strings.Contains(out, "shithub-diff-add") || !strings.Contains(out, "shithub-diff-del") {
		t.Errorf("missing add/del classes")
	}
	if !strings.Contains(out, "@@ -1,3 +1,4 @@") {
		t.Errorf("missing hunk header")
	}
	if !strings.Contains(out, "new second") || !strings.Contains(out, "old second") {
		t.Errorf("missing line content")
	}
}

func TestSplit_PairsAddsAndDeletes(t *testing.T) {
	t.Parallel()
	out := render.Diff(mustParse(t, samplePatch), render.Options{Mode: render.ModeSplit})
	if !strings.Contains(out, "shithub-diff-split") {
		t.Errorf("missing split class")
	}
}

func TestRender_BinaryPlaceholder(t *testing.T) {
	t.Parallel()
	patch := `diff --git a/img.bin b/img.bin
index 1111111..2222222 100644
Binary files a/img.bin and b/img.bin differ
`
	out := render.Diff(mustParse(t, patch), render.Options{})
	if !strings.Contains(out, "shithub-diff-binary") {
		t.Errorf("missing binary placeholder; got: %s", out)
	}
}

func TestRender_RenameHeader(t *testing.T) {
	t.Parallel()
	patch := `diff --git a/old.txt b/new.txt
similarity index 90%
rename from old.txt
rename to new.txt
--- a/old.txt
+++ b/new.txt
@@ -1 +1 @@
-old
+new
`
	out := render.Diff(mustParse(t, patch), render.Options{})
	if !strings.Contains(out, "old.txt → new.txt") {
		t.Errorf("rename header missing; got: %s", out)
	}
	if !strings.Contains(out, "renamed") {
		t.Errorf("rename action missing")
	}
}

func TestRender_TooLargeCollapse(t *testing.T) {
	t.Parallel()
	// Construct a fake hunk with many lines.
	var lines []string
	lines = append(lines, "diff --git a/huge.txt b/huge.txt")
	lines = append(lines, "index 1111111..2222222 100644")
	lines = append(lines, "--- a/huge.txt")
	lines = append(lines, "+++ b/huge.txt")
	header := "@@ -1,1 +1,1100 @@"
	lines = append(lines, header)
	lines = append(lines, " ctx")
	for i := 0; i < 1100; i++ {
		lines = append(lines, "+added")
	}
	patch := strings.Join(lines, "\n") + "\n"
	out := render.Diff(mustParse(t, patch), render.Options{Mode: render.ModeUnified})
	if !strings.Contains(out, "shithub-diff-file-toolarge") {
		t.Errorf("expected too-large collapse; got: %s", out[:200])
	}
}

func TestRender_EmptyDiff(t *testing.T) {
	t.Parallel()
	d, _ := parse.ParseBytes(nil)
	out := render.Diff(d, render.Options{})
	if !strings.Contains(out, "shithub-diff-empty") {
		t.Errorf("expected empty-diff placeholder")
	}
}
