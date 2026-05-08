// SPDX-License-Identifier: AGPL-3.0-or-later

package parse_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos/diff/parse"
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

func TestParse_HappyPath(t *testing.T) {
	t.Parallel()
	d, err := parse.ParseBytes([]byte(samplePatch))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(d.Files) != 1 {
		t.Fatalf("Files = %d, want 1", len(d.Files))
	}
	f := d.Files[0]
	if f.NewPath != "hello.txt" {
		t.Errorf("NewPath = %q", f.NewPath)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("Hunks = %d, want 1", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 3 || h.NewStart != 1 || h.NewLines != 4 {
		t.Errorf("hunk header = %+v", h)
	}
	// Expect: ctx, del, add, add, ctx
	want := []parse.LineKind{
		parse.LineContext, parse.LineDelete, parse.LineAdd, parse.LineAdd, parse.LineContext,
	}
	if len(h.Lines) != len(want) {
		t.Fatalf("Lines len = %d, want %d", len(h.Lines), len(want))
	}
	for i, k := range want {
		if h.Lines[i].Kind != k {
			t.Errorf("Lines[%d].Kind = %v, want %v", i, h.Lines[i].Kind, k)
		}
	}
	// Spot-check line numbers.
	if h.Lines[0].OldLineNo != 1 || h.Lines[0].NewLineNo != 1 {
		t.Errorf("ctx 1: %+v", h.Lines[0])
	}
	if h.Lines[1].OldLineNo != 2 || h.Lines[1].NewLineNo != 0 {
		t.Errorf("del 2: %+v", h.Lines[1])
	}
	if h.Lines[2].OldLineNo != 0 || h.Lines[2].NewLineNo != 2 {
		t.Errorf("add 2: %+v", h.Lines[2])
	}
}

func TestParse_HandlesRename(t *testing.T) {
	t.Parallel()
	patch := `diff --git a/old.txt b/new.txt
similarity index 100%
rename from old.txt
rename to new.txt
`
	d, err := parse.ParseBytes([]byte(patch))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(d.Files) != 1 {
		t.Fatalf("Files = %d", len(d.Files))
	}
	f := d.Files[0]
	if !f.IsRename {
		t.Errorf("IsRename = false")
	}
	if f.OldPath != "old.txt" || f.NewPath != "new.txt" {
		t.Errorf("paths = %q -> %q", f.OldPath, f.NewPath)
	}
}

func TestParse_HandlesBinary(t *testing.T) {
	t.Parallel()
	patch := `diff --git a/img.png b/img.png
index 1111111..2222222 100644
Binary files a/img.png and b/img.png differ
`
	d, err := parse.ParseBytes([]byte(patch))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(d.Files) != 1 || !d.Files[0].IsBinary {
		t.Fatalf("expected one binary file, got %+v", d.Files)
	}
	if len(d.Files[0].Hunks) != 0 {
		t.Errorf("binary file should have no hunks")
	}
}

func TestParse_Empty(t *testing.T) {
	t.Parallel()
	d, err := parse.ParseBytes(nil)
	if err != nil {
		t.Fatalf("ParseBytes nil: %v", err)
	}
	if len(d.Files) != 0 {
		t.Errorf("Files = %d, want 0", len(d.Files))
	}
}

func TestParse_MultipleFiles(t *testing.T) {
	t.Parallel()
	patch := samplePatch + "\n" + strings.Replace(samplePatch, "hello.txt", "world.txt", -1)
	d, err := parse.ParseBytes([]byte(patch))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(d.Files) != 2 {
		t.Errorf("Files = %d, want 2", len(d.Files))
	}
}
