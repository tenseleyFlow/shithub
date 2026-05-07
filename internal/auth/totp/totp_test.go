// SPDX-License-Identifier: AGPL-3.0-or-later

package totp

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAndVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	code, err := Generate(secret, now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	step, err := Verify(secret, code, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if step != Step(now) {
		t.Fatalf("step = %d, want %d", step, Step(now))
	}
}

func TestVerify_AcceptsSkew(t *testing.T) {
	t.Parallel()
	secret, _ := GenerateSecret()
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	codePrev, _ := Generate(secret, t0.Add(-30*time.Second))
	codeNext, _ := Generate(secret, t0.Add(30*time.Second))
	if _, err := Verify(secret, codePrev, t0); err != nil {
		t.Fatalf("prev step rejected: %v", err)
	}
	if _, err := Verify(secret, codeNext, t0); err != nil {
		t.Fatalf("next step rejected: %v", err)
	}
}

func TestVerify_RejectsTooFarOff(t *testing.T) {
	t.Parallel()
	secret, _ := GenerateSecret()
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	old, _ := Generate(secret, t0.Add(-90*time.Second)) // ~3 steps back
	if _, err := Verify(secret, old, t0); err == nil {
		t.Fatal("expected rejection of code 3 steps behind")
	}
}

func TestVerify_RejectsMalformed(t *testing.T) {
	t.Parallel()
	secret, _ := GenerateSecret()
	now := time.Now()
	cases := []string{"", "12345", "1234567", "abcdef", "12345!"}
	for _, c := range cases {
		if _, err := Verify(secret, c, now); err == nil {
			t.Errorf("expected rejection for %q", c)
		}
	}
}

func TestOtpauthURI_Shape(t *testing.T) {
	t.Parallel()
	secret := []byte("0123456789abcdef0123") // 20 bytes
	u := OtpauthURI("shithub", "alice", secret)
	if !strings.HasPrefix(u, "otpauth://totp/shithub:alice?") {
		t.Fatalf("URI shape wrong: %s", u)
	}
	for _, want := range []string{"secret=", "issuer=shithub", "algorithm=SHA1", "digits=6", "period=30"} {
		if !strings.Contains(u, want) {
			t.Errorf("URI missing %q: %s", want, u)
		}
	}
}

func TestEncodeBase32_NoPadding(t *testing.T) {
	t.Parallel()
	enc := EncodeBase32(make([]byte, 20))
	if strings.Contains(enc, "=") {
		t.Fatalf("base32 contains padding: %s", enc)
	}
}

// ----- recovery codes -----

func TestRecoveryCodes_ShapeAndUniqueness(t *testing.T) {
	t.Parallel()
	codes, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("count mismatch")
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !LooksLikeRecoveryCode(c) {
			t.Errorf("LooksLikeRecoveryCode rejected its own output: %q", c)
		}
		if !strings.Contains(c, "-") {
			t.Errorf("missing dashes: %q", c)
		}
		if seen[c] {
			t.Errorf("duplicate code: %q", c)
		}
		seen[c] = true
	}
}

func TestRecoveryCodes_HashRoundTrip(t *testing.T) {
	t.Parallel()
	codes, hashes, _ := GenerateRecoveryCodes()
	for i, c := range codes {
		// Hashing the printed form (with dashes) and the normalized form
		// must yield the same hash.
		if !EqualHash(HashRecoveryCode(c), hashes[i]) {
			t.Errorf("code %q hash mismatch (printed)", c)
		}
		if !EqualHash(HashRecoveryCode(NormalizeRecoveryCode(c)), hashes[i]) {
			t.Errorf("code %q hash mismatch (normalized)", c)
		}
		// Lowercase + space variant should also match.
		messy := "  " + strings.ToLower(c) + "  "
		if !EqualHash(HashRecoveryCode(messy), hashes[i]) {
			t.Errorf("code %q hash mismatch (lowercase + spaces)", c)
		}
	}
}

func TestLooksLikeRecoveryCode(t *testing.T) {
	t.Parallel()
	yes := []string{"ABCD-EFGH-JKMN", "abcd-efgh-jkmn", "A2C4-EF7H-JKM3"}
	for _, s := range yes {
		if !LooksLikeRecoveryCode(s) {
			t.Errorf("expected accept: %q", s)
		}
	}
	no := []string{"123456", "ABC-DEF", "ABCDEFGHJKMN!", ""}
	for _, s := range no {
		if LooksLikeRecoveryCode(s) {
			t.Errorf("expected reject: %q", s)
		}
	}
}

func TestQRSVG_Renders(t *testing.T) {
	t.Parallel()
	secret, _ := GenerateSecret()
	uri := OtpauthURI("shithub", "alice", secret)
	svg, err := QRSVG(uri)
	if err != nil {
		t.Fatalf("QRSVG: %v", err)
	}
	s := string(svg)
	for _, want := range []string{`<svg`, `</svg>`, `viewBox`, `<rect`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in svg", want)
		}
	}
	// Defense-in-depth: secret must NOT appear in the SVG.
	if strings.Contains(s, EncodeBase32(secret)) {
		t.Fatal("secret leaked into rendered SVG")
	}
}
