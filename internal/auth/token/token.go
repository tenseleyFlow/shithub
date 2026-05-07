// SPDX-License-Identifier: AGPL-3.0-or-later

// Package token mints high-entropy random tokens and stores their sha256
// hashes. The pattern: generate a random token, send it in URLs/emails,
// store only the hash. A DB dump leaks no usable tokens; lookups remain
// O(1) via a UNIQUE index on the hash column.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

// SizeBytes is the random payload length. 32 bytes (256 bits) is far
// beyond brute-force budgets for any plausible attacker. base64url-encoded
// it produces a 43-character ASCII-safe URL fragment.
const SizeBytes = 32

// New mints a fresh token. Returns the URL-safe encoding for inclusion in
// emails and links and the sha256 hash for storage in the DB.
func New() (encoded string, hash []byte, err error) {
	raw := make([]byte, SizeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	encoded = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256(raw)
	return encoded, sum[:], nil
}

// HashOf returns the sha256 of the raw bytes encoded in s. Used by lookup
// paths: parse the URL fragment, hash, query by hash.
func HashOf(s string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("token: malformed encoding")
	}
	if len(raw) != SizeBytes {
		return nil, errors.New("token: wrong length")
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// Equal compares two hashes in constant time. Use this instead of
// bytes.Equal anywhere a comparison's timing could leak information.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
