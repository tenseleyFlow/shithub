// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"strings"
	"testing"
)

func TestMarkdownLineDiffSplitsAddedDeletedAndContextRuns(t *testing.T) {
	t.Parallel()

	runs := markdownLineDiff(
		splitMarkdownLines("alpha\nold\nomega\n"),
		splitMarkdownLines("alpha\nnew\nomega\nextra\n"),
	)

	kinds := make([]string, 0, len(runs))
	for _, run := range runs {
		kinds = append(kinds, run.Kind)
	}
	got := strings.Join(kinds, ",")
	want := "context,deleted,added,context,added"
	if got != want {
		t.Fatalf("run kinds = %q, want %q", got, want)
	}
}

func TestRenderMarkdownPreviewDiffBlocksRendersChangedMarkdown(t *testing.T) {
	t.Parallel()

	blocks, err := renderMarkdownPreviewDiffBlocks(
		[]byte("# Demo\n\nold line\n\nkept line\n"),
		[]byte("# Demo\n\nnew line\n\nkept line\n"),
		func(rendered string) string { return rendered },
	)
	if err != nil {
		t.Fatalf("renderMarkdownPreviewDiffBlocks: %v", err)
	}

	var added, deleted bool
	var joined strings.Builder
	for _, block := range blocks {
		if block.Kind == "added" {
			added = true
		}
		if block.Kind == "deleted" {
			deleted = true
		}
		joined.WriteString(string(block.HTML))
	}
	if !added || !deleted {
		t.Fatalf("expected added and deleted blocks, got %#v", blocks)
	}
	html := joined.String()
	for _, want := range []string{"new line", "old line", "<h1"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered diff HTML missing %q: %s", want, html)
		}
	}
}

func TestRenderMarkdownPreviewDiffBlocksSanitizesHTML(t *testing.T) {
	t.Parallel()

	blocks, err := renderMarkdownPreviewDiffBlocks(
		nil,
		[]byte("<script>alert(1)</script>\n\nok\n"),
		func(rendered string) string { return rendered },
	)
	if err != nil {
		t.Fatalf("renderMarkdownPreviewDiffBlocks: %v", err)
	}
	for _, block := range blocks {
		if strings.Contains(string(block.HTML), "<script") {
			t.Fatalf("rendered diff block was not sanitized: %s", block.HTML)
		}
	}
}
