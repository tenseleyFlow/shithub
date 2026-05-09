// SPDX-License-Identifier: AGPL-3.0-or-later

package keyset

import (
	"errors"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Parallel()
	c := Cursor{Value: 1700000000, ID: 42}
	s := Encode(c)
	got, err := Decode(s)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != c {
		t.Errorf("roundtrip = %+v; want %+v", got, c)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{"", "not-base64!", "abc"}
	for _, in := range cases {
		if _, err := Decode(in); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("Decode(%q) err = %v; want ErrInvalidCursor", in, err)
		}
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	t.Parallel()
	key := []byte("super-secret-32-bytes-or-longer-")
	c := Cursor{Value: 99, ID: 1}
	s := Sign(c, key)
	got, err := Verify(s, key)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != c {
		t.Errorf("verify roundtrip = %+v; want %+v", got, c)
	}
}

func TestVerifyRejectsTamper(t *testing.T) {
	t.Parallel()
	key := []byte("k")
	c := Cursor{Value: 1, ID: 2}
	s := Sign(c, key)
	// Flip a body byte at the start.
	bad := strings.Replace(s, s[:4], "AAAA", 1)
	if _, err := Verify(bad, key); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("tampered cursor accepted; want ErrInvalidCursor")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	t.Parallel()
	c := Cursor{Value: 1, ID: 2}
	s := Sign(c, []byte("alice"))
	if _, err := Verify(s, []byte("bob")); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("wrong key accepted; want ErrInvalidCursor")
	}
}

func TestSignPanicsOnEmptyKey(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Sign with empty key did not panic")
		}
	}()
	Sign(Cursor{}, nil)
}
