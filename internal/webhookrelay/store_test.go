// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fx struct {
	deps   webhookrelay.Deps
	userID int64
	pool   *pgxpool.Pool
}

func setup(t testing.TB) fx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	k, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	box, err := secretbox.FromBytes(k)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	return fx{
		// PRO-EXT_SR2-10: Deps.Create now SSRF-validates each
		// destination at creation time. Tests use loopback URLs +
		// the permissive loopbackSSRF() so they keep their existing
		// shape; production deployments use the default (strict).
		deps:   webhookrelay.Deps{Pool: pool, Box: box, SSRF: loopbackSSRF()},
		userID: user.ID,
		pool:   pool,
	}
}

func TestCreate_ReturnsRawTokenAndStoresHash(t *testing.T) {
	t.Parallel()
	f := setup(t)
	in := webhookrelay.CreateInput{
		UserID:     f.userID,
		Name:       "github-mirror",
		HMACSecret: []byte("shared-secret-bytes"),
		Destinations: []webhookrelay.Destination{
			{URL: "http://127.0.0.1:8011/hook"},
			{URL: "http://127.0.0.1:8012/hook"},
		},
	}
	res, err := f.deps.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.RawToken == "" {
		t.Fatal("RawToken should be non-empty")
	}
	if res.Name != "github-mirror" {
		t.Errorf("Name: got %q", res.Name)
	}
	if len(res.Destinations) != 2 {
		t.Errorf("Destinations: got %d, want 2", len(res.Destinations))
	}
	// Look up via the token — proves the stored hash matches mint
	// and the HMAC secret round-trips through AEAD.
	relay, hmac, _, err := f.deps.LookupByToken(context.Background(), res.RawToken)
	if err != nil {
		t.Fatalf("LookupByToken: %v", err)
	}
	if relay.ID != res.ID {
		t.Errorf("LookupByToken id mismatch: got %d, want %d", relay.ID, res.ID)
	}
	if string(hmac) != "shared-secret-bytes" {
		t.Errorf("HMAC roundtrip: got %q, want %q", string(hmac), "shared-secret-bytes")
	}
}

func TestCreate_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, HMACSecret: []byte("k"),
	})
	if !errors.Is(err, webhookrelay.ErrEmptyName) {
		t.Errorf("got %v, want ErrEmptyName", err)
	}
}

func TestCreate_RejectsTooManyDestinations(t *testing.T) {
	t.Parallel()
	f := setup(t)
	dests := make([]webhookrelay.Destination, webhookrelay.MaxDestinations+1)
	for i := range dests {
		dests[i].URL = "http://127.0.0.1:8000/"
	}
	_, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"), Destinations: dests,
	})
	if !errors.Is(err, webhookrelay.ErrTooManyDestinations) {
		t.Errorf("got %v, want ErrTooManyDestinations", err)
	}
}

func TestLookupByToken_UnknownReturnsNotFound(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Mint a fresh token (right shape) that's never been inserted.
	raw, _, _, err := webhookrelay.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, _, _, lookupErr := f.deps.LookupByToken(context.Background(), raw)
	if !errors.Is(lookupErr, webhookrelay.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", lookupErr)
	}
}

func TestLookupByToken_DisabledReturnsErrDisabled(t *testing.T) {
	t.Parallel()
	f := setup(t)
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.deps.Disable(context.Background(), res.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_, _, _, lookupErr := f.deps.LookupByToken(context.Background(), res.RawToken)
	if !errors.Is(lookupErr, webhookrelay.ErrDisabled) {
		t.Errorf("got %v, want ErrDisabled", lookupErr)
	}
}

func TestLookupByToken_MalformedReturnsMalformed(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, _, _, err := f.deps.LookupByToken(context.Background(), "garbage")
	if !errors.Is(err, webhookrelay.ErrMalformed) {
		t.Errorf("got %v, want ErrMalformed", err)
	}
}
