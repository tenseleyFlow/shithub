// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import "testing"

func TestSignAndVerifyRoundtrip(t *testing.T) {
	secret := []byte("super-secret")
	body := []byte(`{"hello":"world"}`)

	sig := SignSHA256(secret, body)
	if !VerifySHA256(secret, body, sig) {
		t.Fatalf("VerifySHA256 returned false on a freshly signed body")
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	secret := []byte("super-secret")
	body := []byte(`{"hello":"world"}`)
	sig := SignSHA256(secret, body)

	tampered := []byte(`{"hello":"WORLD"}`)
	if VerifySHA256(secret, tampered, sig) {
		t.Fatalf("VerifySHA256 accepted tampered body")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"x":1}`)
	sig := SignSHA256([]byte("alice"), body)
	if VerifySHA256([]byte("bob"), body, sig) {
		t.Fatalf("VerifySHA256 accepted wrong secret")
	}
}

func TestSignaturePrefix(t *testing.T) {
	sig := SignSHA256([]byte("k"), []byte("v"))
	if got := sig[:7]; got != "sha256=" {
		t.Fatalf("signature prefix = %q; want %q", got, "sha256=")
	}
}
