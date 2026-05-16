// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

func TestMint_ShapeAndRoundTrip(t *testing.T) {
	t.Parallel()
	raw, hash, prefix, err := webhookrelay.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(raw, webhookrelay.Prefix) {
		t.Errorf("raw should start with prefix %q; got %q", webhookrelay.Prefix, raw)
	}
	if len(raw) != len(webhookrelay.Prefix)+webhookrelay.PayloadLen {
		t.Errorf("raw length: got %d, want %d", len(raw),
			len(webhookrelay.Prefix)+webhookrelay.PayloadLen)
	}
	if len(hash) != 32 {
		t.Errorf("hash length: got %d, want 32 (sha256)", len(hash))
	}
	if !strings.HasPrefix(raw, prefix) {
		t.Errorf("display prefix %q should be a prefix of raw %q", prefix, raw)
	}
	// HashOf(raw) must reproduce the stored hash — confirms the
	// receiver's lookup path matches the mint path bit-for-bit.
	got, err := webhookrelay.HashOf(raw)
	if err != nil {
		t.Fatalf("HashOf round-trip: %v", err)
	}
	if !webhookrelay.EqualHash(got, hash) {
		t.Errorf("HashOf(raw) doesn't match Mint hash")
	}
}

func TestHashOf_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",                               // empty
		"not-a-shithub-token",            // wrong prefix
		webhookrelay.Prefix + "tooshort", // wrong length
		webhookrelay.Prefix + strings.Repeat("!", webhookrelay.PayloadLen),   // bad alphabet
		webhookrelay.Prefix + strings.Repeat("a", webhookrelay.PayloadLen-1), // short by one
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if _, err := webhookrelay.HashOf(c); !errors.Is(err, webhookrelay.ErrMalformed) {
				t.Errorf("HashOf(%q): want ErrMalformed, got %v", c, err)
			}
		})
	}
}

func TestEqualHash_ConstantTime(t *testing.T) {
	t.Parallel()
	// Not a real timing test — just a contract check: equal hashes
	// compare equal; differing hashes compare unequal. The constant-
	// time property is guaranteed by subtle.ConstantTimeCompare.
	a := []byte{1, 2, 3, 4}
	b := []byte{1, 2, 3, 4}
	c := []byte{1, 2, 3, 5}
	if !webhookrelay.EqualHash(a, b) {
		t.Error("EqualHash(a,b): want true")
	}
	if webhookrelay.EqualHash(a, c) {
		t.Error("EqualHash(a,c): want false")
	}
}
