// SPDX-License-Identifier: AGPL-3.0-or-later

package runnerjwt_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
)

func TestDeriveKeyFromTOTPKeyB64_UsesHKDFLabel(t *testing.T) {
	raw := bytesOf(0x42, 32)
	derived, err := runnerjwt.DeriveKeyFromTOTPKeyB64(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("DeriveKeyFromTOTPKeyB64: %v", err)
	}
	if len(derived) != 32 {
		t.Fatalf("derived length: got %d, want 32", len(derived))
	}
	if string(derived) == string(raw) {
		t.Fatal("derived key matched raw TOTP key; want HKDF isolation")
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	signer := newTestSigner(t, now, bytesOf(0x11, 32))

	token, claims, err := signer.Mint(runnerjwt.MintParams{
		RunnerID: 7,
		JobID:    11,
		RunID:    13,
		RepoID:   17,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := len(strings.Split(token, ".")); got != 3 {
		t.Fatalf("token parts: got %d, want 3", got)
	}
	if claims.Exp != now.Add(runnerjwt.DefaultTTL).Unix() {
		t.Fatalf("exp: got %d, want %d", claims.Exp, now.Add(runnerjwt.DefaultTTL).Unix())
	}
	if claims.Purpose != runnerjwt.PurposeAPI {
		t.Fatalf("purpose: got %q, want %q", claims.Purpose, runnerjwt.PurposeAPI)
	}
	runnerID, err := claims.RunnerID()
	if err != nil {
		t.Fatalf("RunnerID: %v", err)
	}
	if runnerID != 7 {
		t.Fatalf("RunnerID: got %d, want 7", runnerID)
	}

	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != claims {
		t.Fatalf("claims mismatch:\n got %#v\nwant %#v", got, claims)
	}
}

func TestMintVerifyCheckoutPurpose(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	signer := newTestSigner(t, now, bytesOf(0x66, 32))

	token, claims, err := signer.Mint(runnerjwt.MintParams{
		RunnerID: 7,
		JobID:    11,
		RunID:    13,
		RepoID:   17,
		Purpose:  runnerjwt.PurposeCheckout,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if claims.Purpose != runnerjwt.PurposeCheckout {
		t.Fatalf("purpose: got %q, want checkout", claims.Purpose)
	}
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Purpose != runnerjwt.PurposeCheckout {
		t.Fatalf("verified purpose: got %q, want checkout", got.Purpose)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	signer := newTestSigner(t, time.Unix(100, 0), bytesOf(0x22, 32))
	token, _, err := signer.Mint(runnerjwt.MintParams{RunnerID: 1, JobID: 2, RunID: 3, RepoID: 4})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(token, ".")
	replacement := "A"
	if parts[1][len(parts[1])-1] == 'A' {
		replacement = "B"
	}
	parts[1] = parts[1][:len(parts[1])-1] + replacement

	if _, err := signer.Verify(strings.Join(parts, ".")); !errors.Is(err, runnerjwt.ErrInvalidSignature) {
		t.Fatalf("Verify tampered payload: got %v, want ErrInvalidSignature", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	issuedAt := time.Unix(100, 0)
	key, err := runnerjwt.DeriveKeyFromTOTPKeyB64(base64.StdEncoding.EncodeToString(bytesOf(0x99, 32)))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	signer, err := runnerjwt.NewFromKey(
		key,
		runnerjwt.WithClock(func() time.Time { return issuedAt }),
		runnerjwt.WithRand(strings.NewReader(string(bytesOf(0x55, 32)))),
	)
	if err != nil {
		t.Fatalf("NewFromKey signer: %v", err)
	}
	token, _, err := signer.Mint(runnerjwt.MintParams{RunnerID: 1, JobID: 2, RunID: 3, RepoID: 4, TTL: time.Second})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	verifier, err := runnerjwt.NewFromKey(key, runnerjwt.WithClock(func() time.Time { return issuedAt.Add(time.Second) }))
	if err != nil {
		t.Fatalf("NewFromKey verifier: %v", err)
	}
	if _, err := verifier.Verify(token); !errors.Is(err, runnerjwt.ErrExpired) {
		t.Fatalf("Verify expired: got %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsUnsupportedHeader(t *testing.T) {
	signer := newTestSigner(t, time.Unix(100, 0), bytesOf(0x44, 32))
	if _, err := signer.Verify("eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.e30.sig"); !errors.Is(err, runnerjwt.ErrUnsupportedHeader) {
		t.Fatalf("Verify unsupported header: got %v, want ErrUnsupportedHeader", err)
	}
}

func TestMintGeneratesDistinctJTI(t *testing.T) {
	now := time.Unix(100, 0)
	rng := strings.NewReader(string(append(bytesOf(0x01, 32), bytesOf(0x02, 32)...)))
	key, err := runnerjwt.DeriveKeyFromTOTPKeyB64(base64.StdEncoding.EncodeToString(bytesOf(0x77, 32)))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	signer, err := runnerjwt.NewFromKey(key, runnerjwt.WithClock(func() time.Time { return now }), runnerjwt.WithRand(rng))
	if err != nil {
		t.Fatalf("NewFromKey: %v", err)
	}
	_, first, err := signer.Mint(runnerjwt.MintParams{RunnerID: 1, JobID: 2, RunID: 3, RepoID: 4})
	if err != nil {
		t.Fatalf("Mint first: %v", err)
	}
	_, second, err := signer.Mint(runnerjwt.MintParams{RunnerID: 1, JobID: 2, RunID: 3, RepoID: 4})
	if err != nil {
		t.Fatalf("Mint second: %v", err)
	}
	if first.JTI == second.JTI {
		t.Fatalf("JTI reused: %s", first.JTI)
	}
}

func newTestSigner(t *testing.T, now time.Time, jtiBytes []byte) *runnerjwt.Signer {
	t.Helper()
	key, err := runnerjwt.DeriveKeyFromTOTPKeyB64(base64.StdEncoding.EncodeToString(bytesOf(0x99, 32)))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	signer, err := runnerjwt.NewFromKey(
		key,
		runnerjwt.WithClock(func() time.Time { return now }),
		runnerjwt.WithRand(strings.NewReader(string(jtiBytes))),
	)
	if err != nil {
		t.Fatalf("NewFromKey: %v", err)
	}
	return signer
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
