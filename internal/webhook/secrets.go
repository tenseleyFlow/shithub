// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
)

// BuildBox constructs the AEAD box used to encrypt/decrypt webhook
// secrets at rest. dedicatedB64 is the base64-encoded 32-byte key
// from cfg.Webhook.AEADKey. Returns nil when the config is empty —
// callers decide what to do (typically: log a loud warning and
// disable webhook delivery for this process).
//
// Before F-shakedown the helper accepted a second "legacy" key to
// decrypt rows written before SHITHUB_WEBHOOK__AEAD_KEY was a
// separate config (they were encrypted under auth.totp_key_b64).
// The prod check confirmed zero rows remained on the legacy key,
// so the fallback was removed.
func BuildBox(dedicatedB64 string) (*secretbox.Box, error) {
	if dedicatedB64 == "" {
		return nil, nil
	}
	box, err := secretbox.FromBase64(dedicatedB64)
	if err != nil {
		return nil, fmt.Errorf("webhook box: %w", err)
	}
	return box, nil
}

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

// OpenSecret decrypts the stored ciphertext+nonce under the
// configured webhook AEAD box. Returns a hard error when the box
// is nil or the decryption fails — the deliverer disables the
// webhook with a precise reason rather than silently signing with
// garbage.
func OpenSecret(box *secretbox.Box, ciphertext, nonce []byte) (string, error) {
	if box == nil {
		return "", errors.New("webhook: no AEAD key configured")
	}
	pt, err := box.Open(ciphertext, nonce)
	if err != nil {
		return "", errors.New("webhook: decrypt failed")
	}
	return string(pt), nil
}
