// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
)

// SecretLength is the byte length of a freshly minted webhook secret.
// 32 bytes ≈ 256 bits — well above HMAC-SHA256's effective security.
const SecretLength = 32

// GenerateSecret returns a hex-encoded random secret suitable for
// dropping into the webhook config form. The hex shape keeps it easy
// to paste into receiver code.
func GenerateSecret() (string, error) {
	b := make([]byte, SecretLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SealSecret AEAD-wraps secret under the supplied box. Returns
// (ciphertext, nonce) for storage in webhooks.secret_ciphertext +
// webhooks.secret_nonce.
func SealSecret(box *secretbox.Box, secret string) (ciphertext, nonce []byte, err error) {
	if box == nil {
		return nil, nil, errors.New("webhook: nil box")
	}
	if secret == "" {
		return nil, nil, errors.New("webhook: empty secret")
	}
	return box.Seal([]byte(secret))
}

// OpenSecret decrypts the stored ciphertext+nonce. Decryption failure
// is a hard error: the deliverer disables the webhook with a precise
// reason rather than silently signing with garbage.
func OpenSecret(box *secretbox.Box, ciphertext, nonce []byte) (string, error) {
	if box == nil {
		return "", errors.New("webhook: nil box")
	}
	pt, err := box.Open(ciphertext, nonce)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
