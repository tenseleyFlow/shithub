// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webhookrelay implements the per-user webhook-relay surface
// shipped by PRO-EXT01-13a: token mint/lookup, the Store CRUD layer,
// ingest (one inbound POST → N pending delivery rows), and the
// outbound Deliver worker (HMAC POST + exponential-backoff retry,
// SSRF-defended).
//
// The token shape mirrors internal/auth/pat: random base62 payload,
// SHA-256 hashed at rest, full raw shown to the user exactly once.
// The relay token is in the URL path of the receiver endpoint
// (POST /webhook-relay/{token}) so users paste a single string into
// upstream services' webhook configs.
package webhookrelay

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Prefix marks all webhook-relay tokens. Mirrors the PAT shape so
// log scrubbers can identify and redact tokens generically.
const Prefix = "shrelay_"

// PayloadLen is the base62 random suffix length. 32 chars of base62
// ≈ 190 bits of entropy — well above the SHA-256 collision floor and
// matches the PAT payload length.
const PayloadLen = 32

// DisplayPrefixLen is the user-facing prefix length. Settings UI
// shows "shrelay_abcd…" so users can distinguish tokens without
// exposing the full secret.
const DisplayPrefixLen = len("shrelay_") + 4

// ErrMalformed signals a token that doesn't pass structural checks
// (prefix, length, alphabet). The receiver returns 404 either way —
// telling the caller "malformed" vs "unknown" would help an attacker
// distinguish probe shapes — but the error type lets callers skip
// the DB lookup for obvious garbage.
var ErrMalformed = errors.New("webhookrelay: malformed token")

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Mint returns a fresh raw token + its SHA-256 hash + display prefix.
// The raw token is shown to the user once and never persisted.
func Mint() (raw string, hash []byte, prefix string, err error) {
	payload, err := randomBase62(PayloadLen)
	if err != nil {
		return "", nil, "", fmt.Errorf("webhookrelay: mint payload: %w", err)
	}
	raw = Prefix + payload
	sum := sha256.Sum256([]byte(raw))
	prefix = raw[:DisplayPrefixLen]
	return raw, sum[:], prefix, nil
}

// HashOf computes the lookup hash for `raw`. Validates prefix/length/
// alphabet first so a malformed input never reaches the DB index.
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

// EqualHash compares two stored hashes in constant time.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func randomBase62(n int) (string, error) {
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

func isBase62(r rune) bool {
	return (r >= '0' && r <= '9') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z')
}
