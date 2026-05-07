// SPDX-License-Identifier: AGPL-3.0-or-later

package secretbox

import (
	"bytes"
	"testing"
)

func mustBox(t *testing.T) *Box {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b, err := FromBytes(k)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	return b
}

func TestSealOpen_RoundTrip(t *testing.T) {
	t.Parallel()
	b := mustBox(t)
	plaintext := []byte("hunter2-totp-secret-bytes")

	ct, nonce, err := b.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, plaintext) {
		t.Fatal("ciphertext contains plaintext substring")
	}
	if len(nonce) != NonceSize {
		t.Fatalf("nonce size = %d, want %d", len(nonce), NonceSize)
	}

	got, err := b.Open(ct, nonce)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open mismatch: %s", got)
	}
}

func TestSealOpen_DifferentNoncesEachCall(t *testing.T) {
	t.Parallel()
	b := mustBox(t)
	plaintext := []byte("same input each time")
	_, n1, _ := b.Seal(plaintext)
	_, n2, _ := b.Seal(plaintext)
	if bytes.Equal(n1, n2) {
		t.Fatal("nonces collided across two Seal calls")
	}
}

func TestOpen_RejectsTamper(t *testing.T) {
	t.Parallel()
	b := mustBox(t)
	ct, nonce, _ := b.Seal([]byte("authentic"))
	ct[0] ^= 0x01
	if _, err := b.Open(ct, nonce); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestOpen_RejectsWrongNonce(t *testing.T) {
	t.Parallel()
	b := mustBox(t)
	ct, nonce, _ := b.Seal([]byte("authentic"))
	otherNonce := make([]byte, len(nonce))
	copy(otherNonce, nonce)
	otherNonce[0] ^= 0x01
	if _, err := b.Open(ct, otherNonce); err == nil {
		t.Fatal("expected error for wrong nonce")
	}
}

func TestOpen_RejectsWrongKey(t *testing.T) {
	t.Parallel()
	a := mustBox(t)
	b := mustBox(t)
	ct, nonce, _ := a.Seal([]byte("alice"))
	if _, err := b.Open(ct, nonce); err == nil {
		t.Fatal("expected error opening with different key")
	}
}

func TestFromBase64_InvalidLength(t *testing.T) {
	t.Parallel()
	if _, err := FromBase64(EncodeKey([]byte("too short"))); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := FromBase64(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestFromBase64_RoundTrip(t *testing.T) {
	t.Parallel()
	k, _ := GenerateKey()
	b1, err := FromBase64(EncodeKey(k))
	if err != nil {
		t.Fatalf("FromBase64: %v", err)
	}
	b2, err := FromBytes(k)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	pt := []byte("alice")
	ct, n, _ := b1.Seal(pt)
	out, err := b2.Open(ct, n)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(out, pt) {
		t.Fatal("round-trip mismatch")
	}
}
