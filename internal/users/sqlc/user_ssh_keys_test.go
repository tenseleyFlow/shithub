// SPDX-License-Identifier: AGPL-3.0-or-later

package usersdb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestGetUserSSHKeyByFingerprintOnlyReturnsAuthenticationKeys(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	q := usersdb.New()

	user, err := q.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "sshlookup",
		DisplayName:  "SSH Lookup",
		PasswordHash: "not-used-in-this-test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	signingFingerprint := "SHA256:signing-only-fingerprint-test"
	if _, err := q.InsertUserSSHKey(ctx, pool, usersdb.InsertUserSSHKeyParams{
		UserID:            user.ID,
		Title:             "signing-only",
		FingerprintSha256: signingFingerprint,
		KeyType:           "ssh-ed25519",
		KeyBits:           0,
		PublicKey:         "ssh-ed25519 AAAAsigning",
		Kind:              "signing",
	}); err != nil {
		t.Fatalf("insert signing key: %v", err)
	}
	if _, err := q.GetUserSSHKeyByFingerprint(ctx, pool, signingFingerprint); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("signing lookup err = %v, want pgx.ErrNoRows", err)
	}

	authFingerprint := "SHA256:authentication-fingerprint-test"
	authKey, err := q.InsertUserSSHKey(ctx, pool, usersdb.InsertUserSSHKeyParams{
		UserID:            user.ID,
		Title:             "authentication",
		FingerprintSha256: authFingerprint,
		KeyType:           "ssh-ed25519",
		KeyBits:           0,
		PublicKey:         "ssh-ed25519 AAAAauth",
		Kind:              "authentication",
	})
	if err != nil {
		t.Fatalf("insert auth key: %v", err)
	}
	got, err := q.GetUserSSHKeyByFingerprint(ctx, pool, authFingerprint)
	if err != nil {
		t.Fatalf("auth lookup: %v", err)
	}
	if got.ID != authKey.ID {
		t.Fatalf("auth lookup id = %d, want %d", got.ID, authKey.ID)
	}
}
