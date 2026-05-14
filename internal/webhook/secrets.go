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

// OpenSecret decrypts the stored ciphertext+nonce. Attempts the
// primary box first; if that fails AND a legacy box is supplied,
// retries under the legacy box. The legacy parameter exists to
// bridge the SHITHUB_WEBHOOK__AEAD_KEY separation: rows written
// before separation were encrypted under auth.totp_key_b64 and
// must still decrypt after the operator generates a dedicated
// webhook key and reads `shithubd admin re-encrypt-webhooks` to
// migrate them. Pass legacy=nil once migration is verified.
//
// Decryption failure under both boxes is a hard error: the
// deliverer disables the webhook with a precise reason rather
// than silently signing with garbage.
func OpenSecret(primary, legacy *secretbox.Box, ciphertext, nonce []byte) (string, error) {
	if primary == nil && legacy == nil {
		return "", errors.New("webhook: no AEAD key configured")
	}
	if primary != nil {
		if pt, err := primary.Open(ciphertext, nonce); err == nil {
			return string(pt), nil
		}
	}
	if legacy != nil {
		if pt, err := legacy.Open(ciphertext, nonce); err == nil {
			return string(pt), nil
		}
	}
	return "", errors.New("webhook: decrypt failed under all configured AEAD keys")
}
