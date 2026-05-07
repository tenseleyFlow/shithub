// SPDX-License-Identifier: AGPL-3.0-or-later

package password

import (
	"strings"
	"testing"
)

// fastParams keeps the test suite quick — argon2id with default cost takes
// hundreds of ms. Real defaults are exercised by the e2e auth integration test.
func fastParams() Params {
	return Params{Memory: 16 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
}

func TestHashAndVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	p := fastParams()
	enc, err := Hash("correct horse battery staple", p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$") {
		t.Fatalf("PHC prefix wrong: %s", enc)
	}
	ok, err := Verify("correct horse battery staple", enc)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify returned false for correct password")
	}
	bad, err := Verify("wrong password........", enc)
	if err != nil {
		t.Fatalf("Verify wrong: %v", err)
	}
	if bad {
		t.Fatal("Verify returned true for wrong password")
	}
}

func TestHash_RejectsTooShort(t *testing.T) {
	t.Parallel()
	if _, err := Hash("short", fastParams()); err == nil {
		t.Fatal("expected error for short password")
	}
}

func TestVerify_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-phc-string",
		"$bcrypt$v=10$xxx",                                       // wrong algo
		"$argon2id$v=18$m=65536,t=3,p=2$AAAAAAAAAAAAAAAA$AAAAAA", // version mismatch
		"$argon2id$v=19$bad-params$AAAAAAAAAAAAAAAA$AAAAAA",
	}
	for _, c := range cases {
		if _, err := Verify("anything", c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestVerifyAgainstDummy_DoesNotPanic(t *testing.T) {
	t.Parallel()
	MustGenerateDummy(fastParams())
	VerifyAgainstDummy("anything")
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	p := fastParams()
	enc, err := Hash("ten-character-password", p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	got, _, _, err := decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Memory != p.Memory || got.Time != p.Time || got.Threads != p.Threads {
		t.Fatalf("params mismatch: got %+v want %+v", got, p)
	}
}
