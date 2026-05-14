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

func TestOpenSecret_RoundTrip(t *testing.T) {
	t.Parallel()
	box := mustBox(t)
	ct, nonce, err := SealSecret(box, "hello")
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	got, err := OpenSecret(box, ct, nonce)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestOpenSecret_FailsUnderUnrelatedKey(t *testing.T) {
	t.Parallel()
	sealed := mustBox(t)
	wrong := mustBox(t)
	ct, nonce, err := SealSecret(sealed, "unreachable")
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	_, err = OpenSecret(wrong, ct, nonce)
	if err == nil {
		t.Fatal("OpenSecret unexpectedly succeeded under an unrelated key")
	}
	if !strings.Contains(err.Error(), "decrypt failed") {
		t.Errorf("error should name the decrypt failure: %v", err)
	}
}

func TestOpenSecret_RefusesNilBox(t *testing.T) {
	t.Parallel()
	_, err := OpenSecret(nil, []byte{0x00}, []byte{0x00})
	if err == nil {
		t.Fatal("OpenSecret unexpectedly succeeded with nil box")
	}
	if !strings.Contains(err.Error(), "no AEAD key configured") {
		t.Errorf("error should name the missing-config condition: %v", err)
	}
}

func TestBuildBox_EmptyKeyReturnsNil(t *testing.T) {
	t.Parallel()
	box, err := BuildBox("")
	if err != nil {
		t.Fatalf("BuildBox(\"\"): %v", err)
	}
	if box != nil {
		t.Fatal("BuildBox(\"\") returned a non-nil box")
	}
}
