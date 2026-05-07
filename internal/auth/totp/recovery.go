// SPDX-License-Identifier: AGPL-3.0-or-later

package totp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"strings"
)

// RecoveryCodeCount is the number of recovery codes generated per user.
const RecoveryCodeCount = 10

// recoveryAlphabet is RFC 4648 base32 minus 0/1/8/B/I/L/O/S so adjacent
// glyphs don't get mistyped. 24 characters is plenty of entropy for our
// per-code length (12 chars → ~57 bits).
const recoveryAlphabet = "ACDEFGHJKMNPQRTUVWXYZ234"

// RecoveryCodeGroups is the number of dash-separated groups in the
// rendered code (currently 3, of 4 chars each → 12 chars total).
const RecoveryCodeGroups = 3

// RecoveryCodeGroupSize is the length of each group.
const RecoveryCodeGroupSize = 4

// GenerateRecoveryCodes mints RecoveryCodeCount fresh codes. Returns the
// human-readable strings (XXXX-XXXX-XXXX) that are shown to the user
// once, and the matching sha256 hashes (stored in the DB).
func GenerateRecoveryCodes() ([]string, [][]byte, error) {
	codes := make([]string, RecoveryCodeCount)
	hashes := make([][]byte, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		c, err := mintCode()
		if err != nil {
			return nil, nil, err
		}
		codes[i] = c
		h := sha256.Sum256([]byte(NormalizeRecoveryCode(c)))
		hashes[i] = h[:]
	}
	return codes, hashes, nil
}

// HashRecoveryCode returns the sha256 hash of the normalized form of
// code, suitable for DB lookup.
func HashRecoveryCode(code string) []byte {
	h := sha256.Sum256([]byte(NormalizeRecoveryCode(code)))
	return h[:]
}

// NormalizeRecoveryCode strips dashes/whitespace and uppercases the
// result so codes typed with stray spaces or lowercase still match.
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// LooksLikeRecoveryCode is a cheap predicate the login challenge handler
// uses to distinguish a recovery code from a TOTP code: alphanumeric and
// of the expected post-normalization length.
func LooksLikeRecoveryCode(s string) bool {
	n := NormalizeRecoveryCode(s)
	if len(n) != RecoveryCodeGroups*RecoveryCodeGroupSize {
		return false
	}
	for _, r := range n {
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isUpper && !isDigit {
			return false
		}
	}
	return true
}

// EqualHash compares two recovery-code hashes in constant time. Used by
// callers that read a candidate hash from the DB and want to compare to
// a value they computed themselves.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// mintCode generates one fresh formatted recovery code.
func mintCode() (string, error) {
	const total = RecoveryCodeGroups * RecoveryCodeGroupSize
	out := make([]byte, total)
	// Generate 5 raw bytes per 4 base32 chars (40 bits → 8 chars; we use
	// our reduced alphabet so generate plenty and slice).
	buf := make([]byte, total*2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := 0; i < total; i++ {
		out[i] = recoveryAlphabet[int(buf[i])%len(recoveryAlphabet)]
	}
	// Insert dashes between groups.
	var b strings.Builder
	for g := 0; g < RecoveryCodeGroups; g++ {
		if g > 0 {
			b.WriteByte('-')
		}
		b.Write(out[g*RecoveryCodeGroupSize : (g+1)*RecoveryCodeGroupSize])
	}
	return b.String(), nil
}

// ErrRecoveryCodeInvalid is returned by callers when the typed code
// doesn't match any stored hash.
var ErrRecoveryCodeInvalid = errors.New("totp: recovery code invalid")

// noOpUseStdEncoding silences unused-import warnings during refactors.
var _ = base32.StdEncoding
