// SPDX-License-Identifier: AGPL-3.0-or-later

package runnertoken_test

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
)

func TestNewAndHashOfRoundTrip(t *testing.T) {
	encoded, hash, err := runnertoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(encoded) != runnertoken.SizeBytes*2 {
		t.Fatalf("encoded length: got %d, want %d", len(encoded), runnertoken.SizeBytes*2)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		t.Fatalf("encoded token is not hex: %v", err)
	}

	got, err := runnertoken.HashOf(encoded)
	if err != nil {
		t.Fatalf("HashOf: %v", err)
	}
	if !runnertoken.Equal(got, hash) {
		t.Fatalf("HashOf did not reproduce stored hash")
	}
	if strings.Contains(hex.EncodeToString(hash), encoded) {
		t.Fatalf("hash contains plaintext token")
	}
}

func TestHashOfRejectsMalformedAndWrongSize(t *testing.T) {
	if _, err := runnertoken.HashOf("not-hex"); !errors.Is(err, runnertoken.ErrMalformed) {
		t.Fatalf("malformed: got %v, want ErrMalformed", err)
	}
	if _, err := runnertoken.HashOf("abcd"); !errors.Is(err, runnertoken.ErrWrongSize) {
		t.Fatalf("wrong size: got %v, want ErrWrongSize", err)
	}
}
