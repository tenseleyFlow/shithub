// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/secretscan"
)

// TestScan_DetectsCuratedPatterns hits each registered pattern with a
// realistic positive sample. Negative tests live in
// TestScan_NoFalsePositiveOnUnrelatedContent below.
//
// IMPORTANT: the sample strings here are syntactically valid but DO
// NOT correspond to live credentials. They are constructed from the
// pattern shape (correct prefix + a deterministic filler) so detection
// can run without ever shipping a real secret in source.
func TestScan_DetectsCuratedPatterns(t *testing.T) {
	t.Parallel()
	// All test fixtures below are deliberately credential-shaped — they
	// are the positive samples the scanner under test must detect.
	// gosec G101 flags the literals; the per-case //nolint directives
	// below mute the noise.
	cases := []struct {
		name        string
		want        string
		fixture     string
		wantExcerpt string
	}{
		{ //nolint:gosec // G101 positive fixture
			name:        "aws-akia",
			want:        "aws-access-key-id",
			fixture:     "AWS_KEY=AKIAIOSFODNN7EXAMPLE",
			wantExcerpt: "AWS_KEY=[REDACTED]",
		},
		{
			name:        "aws-asia",
			want:        "aws-access-key-id",
			fixture:     "tmp=ASIAQQQQQQQQQQQQQQQQ",
			wantExcerpt: "tmp=[REDACTED]",
		},
		{ //nolint:gosec // G101 positive fixture
			name:    "github-pat",
			want:    "github-token",
			fixture: "GH_TOKEN=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		},
		{
			name:    "github-oauth",
			want:    "github-token",
			fixture: "Authorization: token gho_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		},
		{
			name:    "gitlab-pat",
			want:    "gitlab-pat",
			fixture: "GITLAB_TOKEN=glpat-AAAA1111BBBB2222CCCC",
		},
		{
			name: "stripe-live",
			want: "stripe-live-key",
			// Source bytes are split so GitHub's push-protection
			// scanner doesn't pattern-match the literal; the
			// runtime-concatenated value still trips our scanner.
			fixture: "STRIPE_SECRET=sk_" + "live_" + strings.Repeat("a", 24),
		},
		{
			name:    "stripe-test",
			want:    "stripe-test-key",
			fixture: "stripe.api_key = 'sk_" + "test_" + strings.Repeat("a", 24) + "'",
		},
		{
			name:    "slack-bot",
			want:    "slack-token",
			fixture: "slack: xoxb-1234567890-AAAAAAAAAA",
		},
		{ //nolint:gosec // G101 positive fixture; not a real key
			name:    "rsa-private-key",
			want:    "private-key-block",
			fixture: "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----",
		},
		{
			name:    "openssh-private-key",
			want:    "private-key-block",
			fixture: "-----BEGIN OPENSSH PRIVATE KEY-----",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := secretscan.Scan([]byte(tc.fixture), secretscan.ScanOptions{})
			if len(findings) == 0 {
				t.Fatalf("no findings for %s fixture %q", tc.want, tc.fixture)
			}
			seen := false
			for _, f := range findings {
				if f.Pattern == tc.want {
					seen = true
					if tc.wantExcerpt != "" && f.Excerpt != tc.wantExcerpt {
						t.Errorf("excerpt: got %q, want %q", f.Excerpt, tc.wantExcerpt)
					}
					if strings.Contains(f.Excerpt, "AKIA") ||
						strings.Contains(f.Excerpt, "ghp_") ||
						strings.Contains(f.Excerpt, "sk_live_") {
						t.Errorf("excerpt leaked raw secret: %q", f.Excerpt)
					}
				}
			}
			if !seen {
				t.Errorf("expected pattern %q, got %+v", tc.want, findings)
			}
		})
	}
}

// TestScan_RedactsMatchedBytes ensures the Excerpt never carries the
// matched bytes verbatim — the #1 invariant of the scanner.
func TestScan_RedactsMatchedBytes(t *testing.T) {
	t.Parallel()
	content := []byte("api_key=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA hello")
	findings := secretscan.Scan(content, secretscan.ScanOptions{})
	for _, f := range findings {
		if strings.Contains(f.Excerpt, "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
			t.Fatalf("excerpt leaked the matched bytes: %q", f.Excerpt)
		}
		if !strings.Contains(f.Excerpt, "[REDACTED]") {
			t.Fatalf("excerpt missing redaction marker: %q", f.Excerpt)
		}
	}
}

// TestScan_LineNumbersAre1Indexed pins the line-mapping invariant.
func TestScan_LineNumbersAre1Indexed(t *testing.T) {
	t.Parallel()
	content := []byte("line1\nline2\nAWS=AKIAIOSFODNN7EXAMPLE\nline4\n")
	findings := secretscan.Scan(content, secretscan.ScanOptions{})
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Line != 3 {
		t.Errorf("Line: got %d, want 3", findings[0].Line)
	}
}

// TestScan_NoFalsePositiveOnUnrelatedContent makes sure the patterns
// don't match common code shapes. Negative tests below are tighter
// than typical because secret-scan FPs train users to ignore findings.
func TestScan_NoFalsePositiveOnUnrelatedContent(t *testing.T) {
	t.Parallel()
	samples := []string{
		// Hex/base64 that *looks* like an AWS key but lacks the prefix.
		"const HEX = 'BBBBIOSFODNN7EXAMPLE'",
		// String that contains the substring `AKIA` but not as a word boundary.
		"funcAKIAblahblah",
		// GitHub repo URL — has 'ghp' but not the underscore + length.
		"https://github.com/example/repo",
		// A normal 40-char hex string (e.g. SHA-1) is *not* a Stripe key.
		"const sha = '" + strings.Repeat("a", 40) + "'",
		// Slack channel name, not a token.
		"#xoxa-team",
		// A README documenting a private key — no actual PEM block.
		"see also: BEGIN RSA PRIVATE KEY blocks are sensitive",
		// Empty.
		"",
	}
	for i, s := range samples {
		findings := secretscan.Scan([]byte(s), secretscan.ScanOptions{})
		if len(findings) > 0 {
			t.Errorf("sample %d falsely matched: %q → %+v", i, s, findings)
		}
	}
}

// TestScan_MaxBytesTruncates pins the size cap.
func TestScan_MaxBytesTruncates(t *testing.T) {
	t.Parallel()
	content := []byte(strings.Repeat("x", 1000) + "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	findings := secretscan.Scan(content, secretscan.ScanOptions{MaxBytes: 500})
	if len(findings) != 0 {
		t.Errorf("max-bytes cap should have hidden the trailing secret: %+v", findings)
	}
}

// TestScan_EmptyContentReturnsNil pins the empty case.
func TestScan_EmptyContentReturnsNil(t *testing.T) {
	t.Parallel()
	if findings := secretscan.Scan(nil, secretscan.ScanOptions{}); findings != nil {
		t.Errorf("nil content should return nil, got %+v", findings)
	}
	if findings := secretscan.Scan([]byte{}, secretscan.ScanOptions{}); findings != nil {
		t.Errorf("empty content should return nil, got %+v", findings)
	}
}

// TestScan_MultipleFindingsInOneFile pins the multi-match path.
func TestScan_MultipleFindingsInOneFile(t *testing.T) {
	t.Parallel()
	content := []byte("AWS=AKIAIOSFODNN7EXAMPLE\nGH=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n")
	findings := secretscan.Scan(content, secretscan.ScanOptions{})
	if len(findings) != 2 {
		t.Errorf("expected 2 findings, got %d", len(findings))
	}
}

func TestCompileCustomPatternScansAndRedacts(t *testing.T) {
	t.Parallel()
	pattern, err := secretscan.CompileCustomPattern(secretscan.CustomPatternSpec{
		Name:        "internal-token",
		Description: "Internal service token.",
		Pattern:     `shithub_custom_[A-Za-z0-9]{12,}`,
		MinMatchLen: 16,
	})
	if err != nil {
		t.Fatalf("CompileCustomPattern: %v", err)
	}
	findings := secretscan.Scan(
		[]byte("token=shithub_custom_ABCDEF123456\n"),
		secretscan.ScanOptions{Patterns: secretscan.PatternsWithCustom([]secretscan.Pattern{pattern})},
	)
	if len(findings) != 1 {
		t.Fatalf("findings len = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Pattern != "custom/internal-token" {
		t.Fatalf("Pattern = %q, want custom/internal-token", findings[0].Pattern)
	}
	if strings.Contains(findings[0].Excerpt, "shithub_custom_ABCDEF123456") {
		t.Fatalf("excerpt leaked raw custom match: %q", findings[0].Excerpt)
	}
	if !strings.Contains(findings[0].Excerpt, "[REDACTED]") {
		t.Fatalf("excerpt missing redaction marker: %q", findings[0].Excerpt)
	}
}

func TestCompileCustomPatternRejectsBadDefinitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec secretscan.CustomPatternSpec
		want error
	}{
		{
			name: "reserved built-in name",
			spec: secretscan.CustomPatternSpec{Name: "github-token", Pattern: `shithub_custom_[A-Za-z0-9]{12,}`, MinMatchLen: 16},
			want: secretscan.ErrCustomPatternNameReserved,
		},
		{
			name: "invalid name",
			spec: secretscan.CustomPatternSpec{Name: "bad name", Pattern: `shithub_custom_[A-Za-z0-9]{12,}`, MinMatchLen: 16},
			want: secretscan.ErrCustomPatternNameInvalid,
		},
		{
			name: "bad regex",
			spec: secretscan.CustomPatternSpec{Name: "bad-regex", Pattern: `(`, MinMatchLen: 16},
			want: secretscan.ErrCustomPatternExpressionInvalid,
		},
		{
			name: "empty match",
			spec: secretscan.CustomPatternSpec{Name: "empty-match", Pattern: `.*`, MinMatchLen: 16},
			want: secretscan.ErrCustomPatternMatchesEmpty,
		},
		{
			name: "too short minimum",
			spec: secretscan.CustomPatternSpec{Name: "short-min", Pattern: `shithub_custom_[A-Za-z0-9]{12,}`, MinMatchLen: 4},
			want: secretscan.ErrCustomPatternMinMatchInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := secretscan.CompileCustomPattern(tc.spec); !errors.Is(err, tc.want) {
				t.Fatalf("CompileCustomPattern err = %v, want %v", err, tc.want)
			}
		})
	}
}
