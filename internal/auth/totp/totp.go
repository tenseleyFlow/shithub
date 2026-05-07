// SPDX-License-Identifier: AGPL-3.0-or-later

// Package totp implements RFC 6238 TOTP (time-based one-time password)
// enrollment and verification. We use SHA-1 / 30-second period / 6 digits
// to maximize authenticator-app compatibility (every popular app speaks
// these defaults; SHA-256/512 cause real-world surprises).
//
// Counter anti-replay is enforced at the DB layer (BumpTOTPCounter only
// advances last_used_counter when the new step is strictly greater).
package totp

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// SecretSize is the secret length in bytes. 20 is the RFC 4226/6238
// recommendation (160 bits — comfortably above brute-force reach).
const SecretSize = 20

// Period is the time-step in seconds.
const Period uint = 30

// Digits is the number of digits in the generated code.
const Digits = otp.DigitsSix

// SkewSteps is the number of windows ±0 we accept either side of `now`.
// A value of 1 means accept now-30s, now, now+30s — covers normal clock
// drift without expanding the replay window meaningfully.
const SkewSteps uint = 1

// GenerateSecret returns a fresh 20-byte secret.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, SecretSize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("totp: secret: %w", err)
	}
	return b, nil
}

// EncodeBase32 returns the Base32 encoding (no padding) authenticator
// apps display to the user. Used in the otpauth URI.
func EncodeBase32(secret []byte) string {
	return strings.TrimRight(base32.StdEncoding.EncodeToString(secret), "=")
}

// OtpauthURI builds the canonical otpauth:// URI consumed by authenticator
// apps. issuer is the site name (e.g. "shithub"); accountName is the user's
// canonical identifier (we use "shithub:<username>"). The URI carries the
// secret in plaintext — never log it; redactor scrubs it as defense in depth.
func OtpauthURI(issuer, accountName string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", EncodeBase32(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(int(Digits)))
	q.Set("period", strconv.FormatUint(uint64(Period), 10))
	u := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + accountName,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// Generate returns the current 6-digit code for secret. Test affordance —
// not used at runtime by the server, only in tests.
func Generate(secret []byte, t time.Time) (string, error) {
	code, err := totp.GenerateCodeCustom(EncodeBase32(secret), t, totp.ValidateOpts{
		Period:    Period,
		Skew:      SkewSteps,
		Digits:    Digits,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", fmt.Errorf("totp: generate: %w", err)
	}
	return code, nil
}

// Step returns the RFC 6238 step counter for time t.
func Step(t time.Time) int64 {
	return t.Unix() / int64(Period)
}

// Verify checks code against secret with ±SkewSteps tolerance. On
// acceptance, returns the step counter that matched so the caller can
// reject codes whose counter is <= last_used_counter.
//
// Returns (step, nil) on accepted code; (0, err) otherwise. err is
// distinguishable via ErrInvalid for bad codes (vs. real errors).
func Verify(secret []byte, code string, t time.Time) (int64, error) {
	if !looksLikeCode(code) {
		return 0, ErrInvalid
	}
	b32 := EncodeBase32(secret)
	// Walk the skew window ourselves so we know which step matched —
	// pquerna/otp's ValidateCustom returns only a boolean.
	for offset := -int64(SkewSteps); offset <= int64(SkewSteps); offset++ {
		atTime := t.Add(time.Duration(offset) * time.Duration(Period) * time.Second)
		want, err := totp.GenerateCodeCustom(b32, atTime, totp.ValidateOpts{
			Period:    Period,
			Skew:      0,
			Digits:    Digits,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, fmt.Errorf("totp: verify: %w", err)
		}
		if constantTimeEqual(want, code) {
			return Step(atTime), nil
		}
	}
	return 0, ErrInvalid
}

// ErrInvalid means the code doesn't match (or doesn't look like one).
// Distinct from a generation error so callers can branch on it.
var ErrInvalid = errors.New("totp: invalid code")

func looksLikeCode(s string) bool {
	if len(s) != int(Digits) {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
