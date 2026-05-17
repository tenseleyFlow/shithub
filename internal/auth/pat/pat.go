// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pat owns personal-access-token minting, hashing, scope
// constants, and the auth-middleware verification path.
//
// Format: shithub_pat_<32-char-base62>. The fixed `shithub_pat_` prefix is
// recognized by secret-scanning tooling and by our own log redactor — so
// even if someone accidentally pastes a raw token into a log line or a
// public PR, downstream tooling has a chance to spot it.
package pat

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
)

// Prefix is the fixed marker prepended to every minted token.
const Prefix = "shithub_pat_"

// Source discriminates how a user_tokens row was minted. Stored in the
// user_tokens.source column (migration 0103_user_tokens_source.sql).
// New values land here without a DB migration; the column is plain
// text with a Go-side enum.
const (
	// SourceUserCreated is the default — a user pasted the new-token
	// form on /settings/tokens. Pre-S55 rows backfill to this value
	// via the column DEFAULT.
	SourceUserCreated = "user_created"
	// SourceOAuthDevice indicates the token was minted by the
	// internal/auth/devicecode Exchange path on first poll after the
	// user approved a device-flow grant. See `S55-campaign.md`.
	SourceOAuthDevice = "oauth_device"
)

// PayloadLen is the length of the random payload (chars), not bytes.
// 32 base62 characters carry log2(62)*32 ≈ 190 bits of entropy — well
// beyond any plausible brute-force budget.
const PayloadLen = 32

// DisplayPrefixLen is how much of the raw token we keep for display
// (e.g. on the listing page). It includes the literal `shithub_pat_`
// plus four characters of payload, enough to disambiguate at a glance.
const DisplayPrefixLen = len("shithub_pat_") + 4

// MaxTokensPerUser bounds the listing page and the auth-lookup table size.
const MaxTokensPerUser = 50

// alphabet is the base62 character set used for the random payload.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ErrMalformed is returned when the supplied string isn't a well-formed
// shithub PAT. Distinct from "valid format but unknown" so the auth
// middleware can short-circuit without touching the DB.
var ErrMalformed = errors.New("pat: malformed token")

// Mint generates a fresh raw token, its sha256 hash (for storage), and
// its display prefix. The raw token MUST be shown to the user exactly
// once and never stored.
func Mint() (raw string, hash []byte, prefix string, err error) {
	payload, err := randomBase62(PayloadLen)
	if err != nil {
		return "", nil, "", fmt.Errorf("pat: mint payload: %w", err)
	}
	raw = Prefix + payload
	sum := sha256.Sum256([]byte(raw))
	prefix = raw[:DisplayPrefixLen]
	return raw, sum[:], prefix, nil
}

// HashOf computes the canonical lookup hash for raw, after verifying
// the prefix and length. Use this on every middleware lookup so a
// malformed input never reaches the DB index.
func HashOf(raw string) ([]byte, error) {
	if !strings.HasPrefix(raw, Prefix) {
		return nil, ErrMalformed
	}
	if len(raw) != len(Prefix)+PayloadLen {
		return nil, ErrMalformed
	}
	for _, r := range raw[len(Prefix):] {
		if !isBase62(r) {
			return nil, ErrMalformed
		}
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

// EqualHash compares two stored hashes in constant time. The auth path
// already keys by hash via UNIQUE-index lookup, but we also expose this
// for any future code that compares hashes outside a DB lookup.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// LooksLike reports whether s resembles a PAT (cheap structural check).
// Used by URL-credential redaction in the log layer.
func LooksLike(s string) bool {
	if !strings.HasPrefix(s, Prefix) {
		return false
	}
	if len(s) != len(Prefix)+PayloadLen {
		return false
	}
	for _, r := range s[len(Prefix):] {
		if !isBase62(r) {
			return false
		}
	}
	return true
}

func isBase62(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// randomBase62 returns n base62 characters from crypto/rand. Reads more
// bytes than strictly needed and rejection-samples to keep the
// distribution uniform.
func randomBase62(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			// 256 mod 62 == 8; reject the top range to avoid bias.
			if b >= 248 {
				continue
			}
			out = append(out, alphabet[b%62])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
