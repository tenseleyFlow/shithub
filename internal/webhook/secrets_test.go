// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
)

func mustBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b, err := secretbox.FromBytes(key)
	if err != nil {
		t.Fatalf("secretbox.FromBytes: %v", err)
	}
	return b
}

func TestOpenSecret_PrimaryOnly(t *testing.T) {
	t.Parallel()
	primary := mustBox(t)
	ct, nonce, err := SealSecret(primary, "hello")
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	got, err := OpenSecret(primary, nil, ct, nonce)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestOpenSecret_FallsBackToLegacy(t *testing.T) {
	t.Parallel()
	// The pre-separation state: ciphertext was written under the
	// legacy (TOTP-shared) key. After operator splits the AEAD
	// keys, primary is a fresh box that cannot decrypt the
	// legacy ciphertext — the fallback chain must catch this.
	legacy := mustBox(t)
	primary := mustBox(t)
	ct, nonce, err := SealSecret(legacy, "old-row")
	if err != nil {
		t.Fatalf("SealSecret(legacy): %v", err)
	}
	got, err := OpenSecret(primary, legacy, ct, nonce)
	if err != nil {
		t.Fatalf("OpenSecret with fallback: %v", err)
	}
	if got != "old-row" {
		t.Errorf("got %q, want %q", got, "old-row")
	}
}

func TestOpenSecret_FailsWhenNeitherKeyWorks(t *testing.T) {
	t.Parallel()
	legacy := mustBox(t)
	primary := mustBox(t)
	wrong := mustBox(t)
	ct, nonce, err := SealSecret(wrong, "unreachable")
	if err != nil {
		t.Fatalf("SealSecret(wrong): %v", err)
	}
	_, err = OpenSecret(primary, legacy, ct, nonce)
	if err == nil {
		t.Fatal("OpenSecret unexpectedly succeeded under unrelated keys")
	}
	if !strings.Contains(err.Error(), "decrypt failed under all") {
		t.Errorf("error should name the all-keys-failed condition: %v", err)
	}
}

func TestOpenSecret_RefusesAllNilBoxes(t *testing.T) {
	t.Parallel()
	_, err := OpenSecret(nil, nil, []byte{0x00}, []byte{0x00})
	if err == nil {
		t.Fatal("OpenSecret unexpectedly succeeded with both boxes nil")
	}
	if !strings.Contains(err.Error(), "no AEAD key configured") {
		t.Errorf("error should name the missing-config condition: %v", err)
	}
}

func TestOpenSecret_PrimarySucceedsBeforeLegacyTried(t *testing.T) {
	t.Parallel()
	// Once a row has been re-encrypted under the primary key,
	// the legacy box is unused. The chain should short-circuit
	// on primary success — verify by passing a legacy box whose
	// keys would NOT decrypt the ciphertext.
	primary := mustBox(t)
	legacy := mustBox(t)
	ct, nonce, err := SealSecret(primary, "migrated")
	if err != nil {
		t.Fatalf("SealSecret(primary): %v", err)
	}
	got, err := OpenSecret(primary, legacy, ct, nonce)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if got != "migrated" {
		t.Errorf("got %q, want %q", got, "migrated")
	}
}
