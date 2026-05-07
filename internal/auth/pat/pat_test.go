// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestMint_FormatAndUniqueness(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, prefix, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if !strings.HasPrefix(raw, Prefix) {
			t.Fatalf("missing prefix: %s", raw)
		}
		if len(raw) != len(Prefix)+PayloadLen {
			t.Fatalf("wrong length: %d", len(raw))
		}
		if !strings.HasPrefix(prefix, Prefix) {
			t.Fatalf("display prefix wrong: %s", prefix)
		}
		if len(prefix) != DisplayPrefixLen {
			t.Fatalf("display prefix length: %d", len(prefix))
		}
		if seen[raw] {
			t.Fatalf("duplicate raw token: %s", raw)
		}
		seen[raw] = true
		want := sha256.Sum256([]byte(raw))
		if !EqualHash(want[:], hash) {
			t.Fatalf("hash mismatch")
		}
	}
}

func TestHashOf_RoundTrip(t *testing.T) {
	t.Parallel()
	raw, hash, _, _ := Mint()
	got, err := HashOf(raw)
	if err != nil {
		t.Fatalf("HashOf: %v", err)
	}
	if !EqualHash(got, hash) {
		t.Fatalf("hash round-trip mismatch")
	}
}

func TestHashOf_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-pat",
		Prefix + "tooshort",
		Prefix + strings.Repeat("a", PayloadLen-1),
		Prefix + strings.Repeat("a", PayloadLen+1),
		Prefix + strings.Repeat("!", PayloadLen),
	}
	for _, c := range cases {
		if _, err := HashOf(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestLooksLike(t *testing.T) {
	t.Parallel()
	raw, _, _, _ := Mint()
	if !LooksLike(raw) {
		t.Fatal("LooksLike rejected its own output")
	}
	if LooksLike("not-a-pat") {
		t.Fatal("LooksLike accepted nonsense")
	}
}

// ----- scopes -----

func TestHasScope(t *testing.T) {
	t.Parallel()
	if !HasScope([]string{"repo:read"}, ScopeRepoRead) {
		t.Fatal("repo:read should grant repo:read")
	}
	if !HasScope([]string{"repo:write"}, ScopeRepoRead) {
		t.Fatal("repo:write should imply repo:read")
	}
	if HasScope([]string{"repo:read"}, ScopeRepoWrite) {
		t.Fatal("repo:read should NOT imply repo:write")
	}
	if !HasScope([]string{"user:write"}, ScopeUserRead) {
		t.Fatal("user:write should imply user:read")
	}
	if HasScope(nil, ScopeRepoRead) {
		t.Fatal("empty held should grant nothing")
	}
}

func TestNormalizeScopes(t *testing.T) {
	t.Parallel()
	in := []string{"user:read", "bogus", "repo:read", "repo:read", "user:read"}
	got := NormalizeScopes(in)
	want := []string{"repo:read", "user:read"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// ----- debouncer -----

func TestDebouncer_FirstCallTouchesAndSubsequentSuppresses(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60 * time.Second)
	if !d.ShouldTouch(1) {
		t.Fatal("first call must touch")
	}
	if d.ShouldTouch(1) {
		t.Fatal("second call within window must suppress")
	}
}

func TestDebouncer_DifferentTokensIndependent(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60 * time.Second)
	if !d.ShouldTouch(1) {
		t.Fatal("first call for 1")
	}
	if !d.ShouldTouch(2) {
		t.Fatal("first call for 2")
	}
}

func TestDebouncer_WindowReset(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(50 * time.Millisecond)
	if !d.ShouldTouch(1) {
		t.Fatal("first")
	}
	time.Sleep(80 * time.Millisecond)
	if !d.ShouldTouch(1) {
		t.Fatal("after window")
	}
}

func TestDebouncer_HighRate(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60 * time.Second)
	touches := 0
	for i := 0; i < 100; i++ {
		if d.ShouldTouch(42) {
			touches++
		}
	}
	if touches != 1 {
		t.Fatalf("got %d touches over 100 calls, want 1", touches)
	}
}

func TestDebouncer_Forget(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60 * time.Second)
	d.ShouldTouch(1)
	d.Forget(1)
	if !d.ShouldTouch(1) {
		t.Fatal("after Forget, ShouldTouch must return true again")
	}
}
