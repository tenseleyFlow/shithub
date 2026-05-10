// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"strings"
	"testing"
)

func TestRewriteMarkdownRelativeURLs(t *testing.T) {
	t.Parallel()
	in := `<p align="center"><img src="internal/web/static/logo/shithub-mark.svg" width="120"></p>` +
		`<p><a href="CONTRIBUTING.md">docs</a> <a href="#why">why</a> <a href="https://example.com/x">external</a></p>`
	got := rewriteMarkdownRelativeURLs(
		in,
		codeRouteBase("tenseleyFlow", "shithub", "blob", "trunk", ""),
		codeRouteBase("tenseleyFlow", "shithub", "blob", "trunk", ""),
		codeRouteBase("tenseleyFlow", "shithub", "raw", "trunk", ""),
		codeRouteBase("tenseleyFlow", "shithub", "raw", "trunk", ""),
	)
	for _, want := range []string{
		`align="center"`,
		`src="/tenseleyFlow/shithub/raw/trunk/internal/web/static/logo/shithub-mark.svg"`,
		`width="120"`,
		`href="/tenseleyFlow/shithub/blob/trunk/CONTRIBUTING.md"`,
		`href="#why"`,
		`href="https://example.com/x"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestRewriteMarkdownRelativeURLsFromSubdirectory(t *testing.T) {
	t.Parallel()
	got := rewriteMarkdownRelativeURLs(
		`<p><img src="../assets/logo mark.svg"><a href="./guide.md#install">guide</a></p>`,
		codeRouteBase("octo", "demo", "blob", "feature/x", "docs/reference"),
		codeRouteBase("octo", "demo", "blob", "feature/x", ""),
		codeRouteBase("octo", "demo", "raw", "feature/x", "docs/reference"),
		codeRouteBase("octo", "demo", "raw", "feature/x", ""),
	)
	for _, want := range []string{
		`src="/octo/demo/raw/feature/x/docs/assets/logo%20mark.svg"`,
		`href="/octo/demo/blob/feature/x/docs/reference/guide.md#install"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestRewriteRelativeMarkdownURLDoesNotEscapeRepoRoot(t *testing.T) {
	t.Parallel()
	got := rewriteRelativeMarkdownURL(
		"../../../../settings",
		codeRouteBase("octo", "demo", "blob", "trunk", "docs"),
		codeRouteBase("octo", "demo", "blob", "trunk", ""),
	)
	if got != "../../../../settings" {
		t.Fatalf("escaped repo-root link should be left alone, got %q", got)
	}
}

func TestRewriteRelativeMarkdownURL(t *testing.T) {
	t.Parallel()
	got := rewriteRelativeMarkdownURL(
		"internal/web/static/logo/shithub-mark.svg",
		codeRouteBase("tenseleyFlow", "shithub", "raw", "trunk", ""),
		codeRouteBase("tenseleyFlow", "shithub", "raw", "trunk", ""),
	)
	want := "/tenseleyFlow/shithub/raw/trunk/internal/web/static/logo/shithub-mark.svg"
	if got != want {
		t.Fatalf("rewrite = %q, want %q", got, want)
	}
}
