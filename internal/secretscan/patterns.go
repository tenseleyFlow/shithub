// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretscan provides a curated, low-false-positive pattern
// engine for detecting common credential formats committed to a repo.
// The engine is intentionally conservative — we prefer to miss a
// rare/custom secret format than to flood the user with noise that
// trains them to ignore findings.
//
// Patterns here cover well-known formats with stable prefixes or
// unambiguous shapes:
//
//	AWS access keys (AKIA / ASIA / AGPA / AROA / AIDA / ANPA / ANVA / ASCA / APKA prefixes)
//	GitHub personal access / OAuth / app / refresh tokens (ghp_/gho_/ghs_/ghu_/ghr_)
//	GitLab personal access tokens (glpat-...)
//	Stripe live / test API keys (sk_live_ / sk_test_)
//	Slack legacy tokens (xoxa-, xoxb-, xoxp-, xoxr-, xoxs-)
//	Private key headers (-----BEGIN ... PRIVATE KEY-----)
//
// Future PRs extend this set. Non-prefixed high-entropy heuristics
// (40-char base64 strings, hex blobs named "secret", etc.) are
// intentionally NOT in this PR — they are FP-prone and need a
// separate UX (allowlist + confidence levels) which 10c will add.
package secretscan

import "regexp"

// Pattern is one named credential format. Re is compiled once at
// package-init time and is safe for concurrent use.
type Pattern struct {
	// Name is the human-friendly label surfaced in findings and the
	// allowlist UI. Stable: tools and storage rows key off this value,
	// so renames are migrations.
	Name string
	// Description is the longer-form tooltip / runbook text.
	Description string
	// Re is the compiled pattern.
	Re *regexp.Regexp
	// MinMatchLen is a defense against pathological short matches a
	// pattern might allow. Findings below this length are dropped.
	// Zero means no minimum.
	MinMatchLen int
}

// Patterns is the full pattern set. Append-only across releases —
// renaming or removing an entry invalidates allowlist rows that
// reference the Name.
var Patterns = []Pattern{
	{
		Name:        "aws-access-key-id",
		Description: "AWS access key identifier (root / IAM / temporary credentials).",
		// AKIA = long-term IAM, ASIA = STS, AROA = role, AGPA = group,
		// AIDA = IAM user, ANPA / ANVA = service-account / vpc-endpoint,
		// ASCA = certificate, APKA = public key. Documented at
		// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html.
		Re: regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AGPA|AIDA|ANPA|ANVA|ASCA|APKA)[0-9A-Z]{16}\b`),
	},
	{
		Name:        "github-token",
		Description: "GitHub personal access, OAuth, app, server-to-server, or refresh token.",
		// ghp = personal access; gho = oauth; ghu = user-to-server;
		// ghs = server-to-server; ghr = refresh. Each is followed by
		// 36+ base62 chars. https://github.blog/2021-04-05-behind-githubs-new-authentication-token-formats/
		Re: regexp.MustCompile(`\bgh[poursu]_[A-Za-z0-9]{36,251}\b`),
	},
	{
		Name:        "gitlab-pat",
		Description: "GitLab personal access token.",
		// glpat-<20-char base64url>; we accept up to 64 to absorb
		// future-length changes without re-releasing.
		Re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,64}\b`),
	},
	{
		Name:        "stripe-live-key",
		Description: "Stripe live-mode secret API key.",
		// sk_live_<24+ alphanum>. Documented length is 24 base62 chars
		// after the prefix; some restricted keys are longer.
		Re: regexp.MustCompile(`\bsk_live_[A-Za-z0-9]{24,99}\b`),
	},
	{
		Name:        "stripe-test-key",
		Description: "Stripe test-mode secret API key.",
		Re:          regexp.MustCompile(`\bsk_test_[A-Za-z0-9]{24,99}\b`),
	},
	{
		Name:        "slack-token",
		Description: "Slack legacy bot / user / app / workspace token.",
		// xox{a,b,p,r,s}-<digits>-<digits>-<digits>-<hex>; we match the
		// prefix shape with reasonable bounds. The full triple-segment
		// form is shipped as one liberal regex; specific shape checks
		// can come later.
		Re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,100}\b`),
	},
	{
		Name:        "private-key-block",
		Description: "PEM-formatted private key header (RSA, EC, DSA, OpenSSH, generic).",
		// Matches the BEGIN line; the body / END line are not
		// included in the match so the redaction is straightforward.
		Re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED |)PRIVATE KEY( BLOCK)?-----`),
	},
}
