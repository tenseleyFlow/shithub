// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SignSHA256 returns the X-Shithub-Signature-256 header value for the
// given body and per-webhook secret. The format mirrors GitHub's
// (`sha256=<hex>`) so existing receiver libraries verify cleanly.
func SignSHA256(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySHA256 returns true when sig matches HMAC-SHA256(secret, body).
// Uses constant-time compare so the verifier doesn't leak timing info.
// Provided as the receiver-side helper that test code (and any future
// inbound webhook surface) can reuse.
func VerifySHA256(secret, body []byte, sig string) bool {
	want := SignSHA256(secret, body)
	if len(want) != len(sig) {
		return false
	}
	return hmac.Equal([]byte(want), []byte(sig))
}
