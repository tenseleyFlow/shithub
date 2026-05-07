// SPDX-License-Identifier: AGPL-3.0-or-later

package templates_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos/templates"
)

func TestLicenses_AtLeastTen(t *testing.T) {
	t.Parallel()
	got := templates.Licenses()
	if len(got) < 10 {
		t.Fatalf("expected ≥10 licenses, got %d: %v", len(got), got)
	}
	for _, key := range []string{"MIT", "Apache-2.0", "AGPL-3.0", "GPL-3.0", "BSD-3-Clause"} {
		if !templates.HasLicense(key) {
			t.Errorf("missing required license %q", key)
		}
	}
}

func TestGitignores_AtLeastTen(t *testing.T) {
	t.Parallel()
	got := templates.Gitignores()
	if len(got) < 10 {
		t.Fatalf("expected ≥10 gitignores, got %d: %v", len(got), got)
	}
	for _, key := range []string{"Go", "Node", "Python", "Rust", "macOS"} {
		if !templates.HasGitignore(key) {
			t.Errorf("missing required gitignore %q", key)
		}
	}
}

func TestLicenseText_SubstitutesYearAndAuthor(t *testing.T) {
	t.Parallel()
	body, err := templates.LicenseText("MIT", 2026, "Alice Anderson")
	if err != nil {
		t.Fatalf("LicenseText: %v", err)
	}
	if !strings.Contains(body, "2026") {
		t.Errorf("year 2026 not substituted: %s", body[:200])
	}
	if !strings.Contains(body, "Alice Anderson") {
		t.Errorf("author not substituted: %s", body[:200])
	}
	if strings.Contains(body, "<year>") || strings.Contains(body, "[year]") {
		t.Errorf("template placeholders survived substitution: %s", body[:200])
	}
}

func TestLicenseText_RejectsUnknown(t *testing.T) {
	t.Parallel()
	if _, err := templates.LicenseText("WTFPL", 2026, "x"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestGitignoreText_NonEmpty(t *testing.T) {
	t.Parallel()
	body, err := templates.GitignoreText("Go")
	if err != nil {
		t.Fatalf("GitignoreText: %v", err)
	}
	if !strings.Contains(body, ".exe") && !strings.Contains(body, "*.test") {
		t.Errorf("Go gitignore looks empty / wrong: %q", body[:min(200, len(body))])
	}
}

func TestReadmeText_WithAndWithoutDescription(t *testing.T) {
	t.Parallel()
	if got := templates.ReadmeText("foo", ""); got != "# foo\n" {
		t.Errorf("ReadmeText empty desc = %q", got)
	}
	if got := templates.ReadmeText("foo", "hello"); got != "# foo\n\nhello\n" {
		t.Errorf("ReadmeText with desc = %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
