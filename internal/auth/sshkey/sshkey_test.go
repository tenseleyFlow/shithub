// SPDX-License-Identifier: AGPL-3.0-or-later

package sshkey

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestParse_AcceptsEd25519(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ed25519.pub")
	got, err := Parse("my laptop", pub)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != "ssh-ed25519" {
		t.Fatalf("type = %q", got.Type)
	}
	if !strings.HasPrefix(got.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint shape: %q", got.Fingerprint)
	}
	// Cross-check against ssh-keygen if available — defensive.
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		out, err := exec.Command("ssh-keygen", "-E", "sha256", "-lf",
			filepath.Join("testdata", "ed25519.pub")).Output()
		if err != nil {
			t.Fatalf("ssh-keygen: %v", err)
		}
		if !strings.Contains(string(out), got.Fingerprint) {
			t.Fatalf("fingerprint mismatch with ssh-keygen: %q vs %s",
				got.Fingerprint, out)
		}
	}
}

func TestParse_AcceptsRSA2048(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "rsa2048.pub")
	got, err := Parse("ci runner", pub)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != "ssh-rsa" {
		t.Fatalf("type = %q", got.Type)
	}
	if got.Bits < 2048 {
		t.Fatalf("bits = %d", got.Bits)
	}
}

func TestParse_AcceptsECDSA(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ecdsa256.pub")
	got, err := Parse("hsm", pub)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != "ecdsa-sha2-nistp256" {
		t.Fatalf("type = %q", got.Type)
	}
}

func TestParse_RejectsRSA1024(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "rsa1024.pub")
	_, err := Parse("weak", pub)
	if !errors.Is(err, ErrRSATooShort) {
		t.Fatalf("expected ErrRSATooShort, got %v", err)
	}
}

func TestParse_RejectsUnparseable(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-key",
		"ssh-rsa AAAA broken",
		"ssh-dss AAAAB3NzaC1kc3MAAACB",
	}
	for _, in := range cases {
		_, err := Parse("title", in)
		if err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestParse_RejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ed25519.pub")
	_, err := Parse("   ", pub)
	if !errors.Is(err, ErrTitleEmpty) {
		t.Fatalf("expected ErrTitleEmpty, got %v", err)
	}
}

func TestParse_RejectsLongTitle(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ed25519.pub")
	_, err := Parse(strings.Repeat("a", 81), pub)
	if !errors.Is(err, ErrTitleTooLong) {
		t.Fatalf("expected ErrTitleTooLong, got %v", err)
	}
}

func TestParse_RejectsControlCharsInTitle(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ed25519.pub")
	_, err := Parse("oops\nnewline", pub)
	if !errors.Is(err, ErrTitleControl) {
		t.Fatalf("expected ErrTitleControl, got %v", err)
	}
}

func TestParse_CanonicalizesPublicKey(t *testing.T) {
	t.Parallel()
	pub := mustRead(t, "ed25519.pub")
	got, _ := Parse("my laptop", pub)
	// No trailing newline, no comment.
	if strings.HasSuffix(got.PublicKey, "\n") {
		t.Fatal("canonical key has trailing newline")
	}
	if strings.Contains(got.PublicKey, "fixture-ed25519") {
		t.Fatal("canonical key includes original comment")
	}
	parts := strings.Fields(got.PublicKey)
	if len(parts) != 2 {
		t.Fatalf("canonical key should be 'algo b64', got %q", got.PublicKey)
	}
}
