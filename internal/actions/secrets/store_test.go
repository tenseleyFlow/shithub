// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// freshKey returns a random base64 32-byte key suitable for
// secretbox.FromBase64. We mint one per-test so test failures don't
// contaminate the next run's key material.
func freshKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type fx struct {
	deps   secrets.Deps
	repoID int64
	userID int64
}

func setup(t *testing.T) fx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	box, err := secretbox.FromBase64(freshKey(t))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	return fx{
		deps: secrets.Deps{
			Pool:   pool,
			Box:    box,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		repoID: repo.ID,
		userID: user.ID,
	}
}

func TestSet_RoundTripsThroughSecretbox(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "MY_SECRET", []byte("hunter2"), f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	plain, err := f.deps.Get(ctx, scope, "MY_SECRET")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(plain) != "hunter2" {
		t.Errorf("got %q want hunter2", string(plain))
	}
}

func TestSet_OverwriteOnSameName(t *testing.T) {
	// Set twice with the same name → second value wins (UPSERT).
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "API_KEY", []byte("v1"), f.userID); err != nil {
		t.Fatalf("Set v1: %v", err)
	}
	if err := f.deps.Set(ctx, scope, "API_KEY", []byte("v2-rotated"), f.userID); err != nil {
		t.Fatalf("Set v2: %v", err)
	}
	plain, err := f.deps.Get(ctx, scope, "API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(plain) != "v2-rotated" {
		t.Errorf("got %q want v2-rotated", string(plain))
	}
}

func TestSet_InvalidNameRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	for _, name := range []string{"", "1leading-digit", "has space", "has-dash", "café"} {
		if err := f.deps.Set(ctx, scope, name, []byte("v"), f.userID); !errors.Is(err, secrets.ErrInvalidName) {
			t.Errorf("Set name=%q: expected ErrInvalidName, got %v", name, err)
		}
	}
}

func TestSet_EmptyValueRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "OK", nil, f.userID); !errors.Is(err, secrets.ErrEmptyValue) {
		t.Errorf("Set empty: expected ErrEmptyValue, got %v", err)
	}
}

func TestSet_InvalidScopeRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, sc := range []secrets.Scope{
		{},                    // both zero
		{RepoID: 1, OrgID: 2}, // both set
	} {
		if err := f.deps.Set(ctx, sc, "K", []byte("v"), 0); !errors.Is(err, secrets.ErrInvalidScope) {
			t.Errorf("Set scope=%+v: expected ErrInvalidScope, got %v", sc, err)
		}
	}
}

func TestList_NamesAndMetadataOnly(t *testing.T) {
	// Pin the security-load-bearing invariant: List returns no plaintext
	// or ciphertext. The web UI consumes Meta; the leak surface should be
	// zero by construction.
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	for _, n := range []string{"AAA", "BBB", "ccc"} {
		if err := f.deps.Set(ctx, scope, n, []byte("v-"+n), f.userID); err != nil {
			t.Fatalf("Set %s: %v", n, err)
		}
	}
	metas, err := f.deps.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("got %d metas, want 3", len(metas))
	}
	// Sorted alphabetical (case-insensitive citext): AAA < BBB < ccc.
	want := []string{"AAA", "BBB", "ccc"}
	for i, w := range want {
		if metas[i].Name != w {
			t.Errorf("metas[%d].Name = %q, want %q", i, metas[i].Name, w)
		}
	}
}

func TestDelete_RemovesRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "TOKEN", []byte("v"), f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.deps.Delete(ctx, scope, "TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.deps.Get(ctx, scope, "TOKEN"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestDelete_MissingIsIdempotent(t *testing.T) {
	// DELETE WHERE doesn't error on zero rows; the store keeps that
	// surface so callers can call Delete blindly during cleanup.
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Delete(ctx, scope, "NEVER_EXISTED"); err != nil {
		t.Errorf("Delete missing: expected nil, got %v", err)
	}
}

func TestGet_CitextNameIsCaseInsensitive(t *testing.T) {
	// citext column means MY_SECRET and my_secret collide. Pin it.
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "MY_TOKEN", []byte("v"), f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	plain, err := f.deps.Get(ctx, scope, "my_token")
	if err != nil {
		t.Fatalf("Get lowercased: %v", err)
	}
	if string(plain) != "v" {
		t.Errorf("got %q want v", string(plain))
	}
}

// PRO-EXT_SR-08: GetMeta returns the metadata for a single user-scope
// secret in one query, without decrypting. Used by the REST GET-by-name
// handler in place of List + linear scan.
func TestGetMeta_ReturnsRowWithoutDecrypting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.UserScope(f.userID)
	if err := f.deps.Set(ctx, scope, "ALPHA", []byte("v"), f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	meta, err := f.deps.GetMeta(ctx, scope, "ALPHA")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if meta.Name != "ALPHA" {
		t.Errorf("Name: got %q want ALPHA", meta.Name)
	}
	if meta.CreatedByUserID != f.userID {
		t.Errorf("CreatedByUserID: got %d want %d", meta.CreatedByUserID, f.userID)
	}
	if meta.ID == 0 {
		t.Error("ID should be set")
	}
}

func TestGetMeta_MissingReturnsNotFound(t *testing.T) {
	f := setup(t)
	if _, err := f.deps.GetMeta(context.Background(), secrets.UserScope(f.userID), "NEVER_SET"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("GetMeta missing: got %v, want ErrNotFound", err)
	}
}

func TestGetMeta_NonUserScopeRejected(t *testing.T) {
	// GetMeta is user-scope-only for now; repo + org REST GETs still
	// use the legacy List+scan path. Pin the contract so a future
	// refactor either widens it deliberately or stays user-only.
	f := setup(t)
	if _, err := f.deps.GetMeta(context.Background(), secrets.RepoScope(f.repoID), "ANY"); !errors.Is(err, secrets.ErrInvalidScope) {
		t.Fatalf("GetMeta repo-scope: got %v, want ErrInvalidScope", err)
	}
}

// TestCiphertext_IsActuallyEncryptedInDB is the load-bearing pin
// the spec called out: verify via psql that the ciphertext column
// is bytea, not plaintext.
func TestCiphertext_IsActuallyEncryptedInDB(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := secrets.RepoScope(f.repoID)
	plaintext := []byte("the-quick-brown-fox")
	if err := f.deps.Set(ctx, scope, "RAW_PROBE", plaintext, f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var ct []byte
	if err := f.deps.Pool.QueryRow(ctx,
		`SELECT ciphertext FROM workflow_secrets WHERE repo_id = $1 AND name = $2`,
		f.repoID, "RAW_PROBE").Scan(&ct); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("ciphertext is empty")
	}
	for i := 0; i+len(plaintext) <= len(ct); i++ {
		if string(ct[i:i+len(plaintext)]) == string(plaintext) {
			t.Fatal("plaintext appears verbatim in ciphertext bytea — encryption broken")
		}
	}
}
