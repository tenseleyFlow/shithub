// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sealbox owns the server-side X25519 keypair used by the
// `/api/v1/repos/{...}/actions/secrets/public-key` (and org analog)
// endpoint. Clients fetch the public half, encrypt a secret value
// with NaCl sealed-box (anonymous-sender), and PUT the ciphertext.
// The server decodes with the private half on the way in, then hands
// plaintext to the existing `internal/actions/secrets` orchestrator
// which re-encrypts with the symmetric storage key for at-rest.
//
// The keypair is operator-supplied via SHITHUB_ACTIONS__SECRETS__BOX_PRIVATE_KEY_B64
// (base64 of the 32-byte X25519 private key). When unset the server
// generates a per-process keypair and logs a loud warning: secrets
// PUT in one process won't be decryptable by another, so production
// deployments MUST set the key.
//
// The exposed `KeyID` is a deterministic short string derived from the
// public key — clients echo it back on PUT so the server can detect a
// stale public-key cache and reject (rather than silently fail to
// decrypt to garbage).
package sealbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// PrivateKeyLen is the X25519 private-key length in bytes.
const PrivateKeyLen = 32

// PublicKeyLen mirrors PrivateKeyLen — X25519 public keys are the
// same length as private keys.
const PublicKeyLen = 32

// Box holds the server's long-lived X25519 keypair. Construct via
// New() to generate a fresh one (dev/tests) or FromBase64 to load
// operator-supplied material.
//
// Methods are safe for concurrent use — the underlying keys are
// immutable once the Box is built.
type Box struct {
	priv [PrivateKeyLen]byte
	pub  [PublicKeyLen]byte
}

// ErrInvalidPrivateKey signals a malformed base64 or wrong-length
// input to FromBase64.
var ErrInvalidPrivateKey = errors.New("sealbox: invalid private key (want 32 bytes base64)")

// ErrCiphertextMalformed signals a malformed encrypted_value on the
// PUT-secret path. Caller maps this to 400 invalid_request.
var ErrCiphertextMalformed = errors.New("sealbox: ciphertext malformed or invalid base64")

// ErrDecryptFailed signals a ciphertext that decoded but didn't open
// against our keypair — usually a stale public_key on the client.
var ErrDecryptFailed = errors.New("sealbox: decrypt failed (stale public_key?)")

// New generates a fresh X25519 keypair. Use FromBase64 for
// production; New is intended for tests and the dev auto-key path.
func New() (*Box, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sealbox: generate: %w", err)
	}
	return &Box{priv: *priv, pub: *pub}, nil
}

// FromBase64 builds a Box from the base64-encoded 32-byte X25519
// private key. The public half is computed deterministically from
// the private half (NaCl X25519 derivation).
func FromBase64(privB64 string) (*Box, error) {
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return nil, ErrInvalidPrivateKey
	}
	if len(priv) != PrivateKeyLen {
		return nil, ErrInvalidPrivateKey
	}
	var b Box
	copy(b.priv[:], priv)
	// Derive public from private. nacl/box internally does
	// curve25519.ScalarBaseMult(pub, priv); we exposed the same via
	// scalarmult below to avoid an extra import surface.
	if err := derivePublic(&b.pub, &b.priv); err != nil {
		return nil, fmt.Errorf("sealbox: derive public: %w", err)
	}
	return &b, nil
}

// PublicKey returns the 32-byte X25519 public key, intended for the
// `/actions/secrets/public-key` response. The caller base64-encodes
// for transport.
func (b *Box) PublicKey() [PublicKeyLen]byte { return b.pub }

// PublicKeyBase64 is a convenience wrapper for the HTTP layer.
func (b *Box) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(b.pub[:])
}

// KeyID is a deterministic short identifier for the current public
// key. Clients echo it on PUT so the server can detect stale caches.
// Format: first 16 chars of base64(sha256(pubkey)).
func (b *Box) KeyID() string {
	sum := sha256.Sum256(b.pub[:])
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// OpenAnonymous decodes a NaCl sealed-box ciphertext (base64-encoded
// for transport) and returns the plaintext. The "anonymous" form
// embeds an ephemeral sender public key in the ciphertext, so the
// server doesn't need to know who encrypted — only the recipient
// keypair (this Box) is required.
//
// Maps to libsodium's `crypto_box_seal_open` (the inverse of
// `crypto_box_seal` that gh's CLI/curl users invoke).
func (b *Box) OpenAnonymous(encryptedB64 string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return nil, ErrCiphertextMalformed
	}
	out, ok := box.OpenAnonymous(nil, ciphertext, &b.pub, &b.priv)
	if !ok {
		return nil, ErrDecryptFailed
	}
	return out, nil
}
