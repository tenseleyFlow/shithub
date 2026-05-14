// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
)

// BuildBoxes returns the (primary, legacy) pair callers wire into
// ManageDeps / DeliverDeps. Semantics:
//
//   - If dedicatedB64 is set, primary is built from it and legacy
//     is built from legacyB64 (if set). New encryption uses
//     primary; OpenSecret falls back to legacy for rows written
//     before separation.
//   - If dedicatedB64 is empty, the pre-separation state applies:
//     primary IS the legacy box, legacy is nil. There's nothing
//     to fall back to because nothing has split yet.
//
// Either or both inputs may be empty; the helper does not warn —
// callers decide what to do with a nil primary (typically: log a
// loud warning and disable webhook delivery for this process).
func BuildBoxes(dedicatedB64, legacyB64 string) (primary, legacy *secretbox.Box, err error) {
	if legacyB64 != "" {
		legacy, err = secretbox.FromBase64(legacyB64)
		if err != nil {
			return nil, nil, fmt.Errorf("webhook legacy box: %w", err)
		}
	}
	if dedicatedB64 != "" {
		primary, err = secretbox.FromBase64(dedicatedB64)
		if err != nil {
			return nil, nil, fmt.Errorf("webhook primary box: %w", err)
		}
		return primary, legacy, nil
	}
	// Pre-separation: only the shared/legacy key exists. There's
	// no fallback chain — surface a single box as primary.
	return legacy, nil, nil
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
