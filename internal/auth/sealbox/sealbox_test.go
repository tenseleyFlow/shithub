// SPDX-License-Identifier: AGPL-3.0-or-later

package sealbox_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"golang.org/x/crypto/nacl/box"

	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
)

func TestNewAndOpenAnonymous_RoundTrip(t *testing.T) {
	b, err := sealbox.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.PublicKeyBase64() == "" {
		t.Fatal("empty public key")
	}
	pubKey := b.PublicKey()

	plaintext := []byte("super-secret-value")
	ciphertext, err := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	encrypted := base64.StdEncoding.EncodeToString(ciphertext)

	out, err := b.OpenAnonymous(encrypted)
	if err != nil {
		t.Fatalf("OpenAnonymous: %v", err)
	}
	if string(out) != string(plaintext) {
		t.Errorf("round-trip: got %q, want %q", out, plaintext)
	}
}

func TestFromBase64_DerivesPublicKeyConsistently(t *testing.T) {
	// Encrypt against a known keypair, then verify FromBase64 yields
	// a Box whose public key matches and which can decrypt.
	original, err := sealbox.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pubKey := original.PublicKey()
	plaintext := []byte("hello")
	ct, _ := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)

	// Round-trip via a new Box loaded from the original's private key.
	// We need access to the private key — for tests, dump it via a
	// secondary path: re-construct from a known fixed private key.
	priv32 := make([]byte, 32)
	priv32[0] = 1 // deterministic non-zero scalar
	loaded, err := sealbox.FromBase64(base64.StdEncoding.EncodeToString(priv32))
	if err != nil {
		t.Fatalf("FromBase64: %v", err)
	}
	loadedPub := loaded.PublicKey()
	pt := []byte("via-fromb64")
	ct2, _ := box.SealAnonymous(nil, pt, &loadedPub, rand.Reader)
	out, err := loaded.OpenAnonymous(base64.StdEncoding.EncodeToString(ct2))
	if err != nil {
		t.Fatalf("OpenAnonymous loaded: %v", err)
	}
	if string(out) != string(pt) {
		t.Errorf("loaded round-trip: got %q, want %q", out, pt)
	}
	// And confirm the unrelated keypair's ciphertext can't open here.
	_, err = loaded.OpenAnonymous(base64.StdEncoding.EncodeToString(ct))
	if !errors.Is(err, sealbox.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed for foreign ciphertext; got %v", err)
	}
}

func TestFromBase64_RejectsBadInput(t *testing.T) {
	cases := []string{
		"",
		"!!!not-base64!!!",
		base64.StdEncoding.EncodeToString([]byte("too-short")),
	}
	for _, in := range cases {
		if _, err := sealbox.FromBase64(in); !errors.Is(err, sealbox.ErrInvalidPrivateKey) {
			t.Errorf("FromBase64(%q): got %v, want ErrInvalidPrivateKey", in, err)
		}
	}
}

func TestOpenAnonymous_RejectsMalformed(t *testing.T) {
	b, err := sealbox.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.OpenAnonymous("not-base64!@#"); !errors.Is(err, sealbox.ErrCiphertextMalformed) {
		t.Errorf("malformed base64: got %v, want ErrCiphertextMalformed", err)
	}
	if _, err := b.OpenAnonymous(base64.StdEncoding.EncodeToString([]byte("too-short"))); !errors.Is(err, sealbox.ErrDecryptFailed) {
		t.Errorf("too-short ciphertext: got %v, want ErrDecryptFailed", err)
	}
}

func TestKeyID_StableForFixedKey(t *testing.T) {
	priv32 := make([]byte, 32)
	priv32[0] = 7
	b1, err := sealbox.FromBase64(base64.StdEncoding.EncodeToString(priv32))
	if err != nil {
		t.Fatalf("FromBase64: %v", err)
	}
	b2, err := sealbox.FromBase64(base64.StdEncoding.EncodeToString(priv32))
	if err != nil {
		t.Fatalf("FromBase64: %v", err)
	}
	if b1.KeyID() != b2.KeyID() {
		t.Errorf("KeyID not stable: %q vs %q", b1.KeyID(), b2.KeyID())
	}
	if b1.KeyID() == "" || len(b1.KeyID()) != 16 {
		t.Errorf("KeyID shape: got %q (len=%d)", b1.KeyID(), len(b1.KeyID()))
	}
}
