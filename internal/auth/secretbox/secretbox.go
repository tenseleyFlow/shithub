// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretbox is a thin AEAD wrapper used to protect at-rest
// secrets — currently TOTP secrets, future sprints will reuse it for
// per-user key material (PATs, deploy keys, etc.).
//
// Construction: chacha20poly1305 (XChaCha20 not used — we mint nonces
// per-row and store them, so the smaller 12-byte nonce is fine).
//
// Key sourcing: 32-byte key, base64-encoded in config (auth.totp_key_b64
// or env SHITHUB_TOTP_KEY). Operators rotate by re-encrypting all rows —
// a key change without a rotation breaks every existing 2FA login. That
// procedure is documented in docs/internal/2fa.md.
package secretbox

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the required key length in bytes.
const KeySize = chacha20poly1305.KeySize // 32

// NonceSize is the AEAD nonce length, stored alongside the ciphertext.
const NonceSize = chacha20poly1305.NonceSize // 12

// Box wraps a ready-to-use AEAD. Construct with FromBase64 or FromBytes.
type Box struct {
	aead cipher.AEAD
}

// FromBase64 decodes the supplied base64 key (standard or url alphabet)
// and returns a Box. Returns an error if the key isn't 32 bytes.
func FromBase64(b64 string) (*Box, error) {
	if b64 == "" {
		return nil, errors.New("secretbox: empty key")
	}
	raw, err := decodeKey(b64)
	if err != nil {
		return nil, fmt.Errorf("secretbox: decode key: %w", err)
	}
	return FromBytes(raw)
}

// FromBytes wraps raw key bytes. Returns an error if not exactly KeySize.
func FromBytes(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", KeySize, len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: aead: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext under a freshly minted random nonce. Returns
// (ciphertext, nonce). Nonces are 12 bytes — at our scale the birthday
// bound is well above the operational lifetime of any single key.
func (b *Box) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	ciphertext = b.aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// Open decrypts ciphertext with the stored nonce. Authenticated decryption
// failures (tamper, wrong key) return a non-nil error.
func (b *Box) Open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("secretbox: nonce must be %d bytes, got %d", NonceSize, len(nonce))
	}
	pt, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secretbox: open: %w", err)
	}
	return pt, nil
}

// GenerateKey returns a fresh 32-byte key suitable for FromBytes /
// FromBase64. Used by the test harness and an operator helper.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// EncodeKey returns the standard-base64 encoding suitable for placing in
// auth.totp_key_b64.
func EncodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

func decodeKey(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
