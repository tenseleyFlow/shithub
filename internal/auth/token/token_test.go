// SPDX-License-Identifier: AGPL-3.0-or-later

package token

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestNew_Unique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		enc, h, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if seen[enc] {
			t.Fatalf("duplicate encoded token: %s", enc)
		}
		seen[enc] = true
		if len(h) != sha256.Size {
			t.Fatalf("hash size = %d, want %d", len(h), sha256.Size)
		}
		// HashOf(encoded) must reproduce the same hash.
		got, err := HashOf(enc)
		if err != nil {
			t.Fatalf("HashOf: %v", err)
		}
		if !Equal(got, h) {
			t.Fatalf("HashOf mismatch")
		}
	}
}

func TestHashOf_RejectsMalformed(t *testing.T) {
	t.Parallel()
	if _, err := HashOf("!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for invalid b64")
	}
	short := base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := HashOf(short); err == nil {
		t.Fatal("expected error for wrong length")
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a := []byte{1, 2, 3, 4}
	b := []byte{1, 2, 3, 4}
	c := []byte{1, 2, 3, 5}
	if !Equal(a, b) {
		t.Fatal("Equal(a,b) = false, want true")
	}
	if Equal(a, c) {
		t.Fatal("Equal(a,c) = true, want false")
	}
}
