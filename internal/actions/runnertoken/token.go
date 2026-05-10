// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runnertoken mints and hashes long-lived runner registration tokens.
//
// Tokens are 32 random bytes rendered as hex for operator copy/paste. Only the
// SHA-256 hash is stored in runner_tokens; the plaintext is printed once by
// `shithubd admin actions runner register` and then lost.
package runnertoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

const SizeBytes = 32

var (
	ErrMalformed = errors.New("runnertoken: malformed token")
	ErrWrongSize = errors.New("runnertoken: wrong token length")
)

// New mints a token and returns the hex encoding plus its SHA-256 hash.
func New() (encoded string, hash []byte, err error) {
	raw := make([]byte, SizeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	encoded = hex.EncodeToString(raw)
	sum := sha256.Sum256(raw)
	return encoded, sum[:], nil
}

// HashOf decodes a hex registration token and returns the stored hash.
func HashOf(encoded string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, ErrMalformed
	}
	if len(raw) != SizeBytes {
		return nil, ErrWrongSize
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// Equal compares two token hashes in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
