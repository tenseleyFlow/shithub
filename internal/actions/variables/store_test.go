// SPDX-License-Identifier: AGPL-3.0-or-later

package variables_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/variables"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fx struct {
	deps   variables.Deps
	repoID int64
	orgID  int64
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
	org, err := orgsdb.New().CreateOrg(ctx, pool, orgsdb.CreateOrgParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		Description:     "",
		BillingEmail:    "ops@example.com",
		CreatedByUserID: pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	return fx{
		deps:   variables.Deps{Pool: pool},
		repoID: repo.ID,
		orgID:  org.ID,
		userID: user.ID,
	}
}

func TestSetGet_RoundTripRepoScope(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "IMAGE_TAG", "2026.05", f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := f.deps.Get(ctx, scope, "IMAGE_TAG")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "2026.05" {
		t.Errorf("got %q want 2026.05", got.Value)
	}
	if got.CreatedByUserID != f.userID {
		t.Errorf("CreatedByUserID = %d, want %d", got.CreatedByUserID, f.userID)
	}
}

func TestSet_AllowsEmptyValue(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "EMPTY", "", f.userID); err != nil {
		t.Fatalf("Set empty value: %v", err)
	}
	got, err := f.deps.Get(ctx, scope, "EMPTY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "" {
		t.Errorf("got %q want empty string", got.Value)
	}
}

func TestSet_OverwriteOnSameName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "TARGET_ENV", "staging", f.userID); err != nil {
		t.Fatalf("Set staging: %v", err)
	}
	if err := f.deps.Set(ctx, scope, "TARGET_ENV", "production", f.userID); err != nil {
		t.Fatalf("Set production: %v", err)
	}
	got, err := f.deps.Get(ctx, scope, "TARGET_ENV")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "production" {
		t.Errorf("got %q want production", got.Value)
	}
}

func TestSet_InvalidNameRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	for _, name := range []string{"", "1leading-digit", "has space", "has-dash", "cafe!"} {
		if err := f.deps.Set(ctx, scope, name, "v", f.userID); !errors.Is(err, variables.ErrInvalidName) {
			t.Errorf("Set name=%q: expected ErrInvalidName, got %v", name, err)
		}
	}
}

func TestSet_ValueTooLongRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	value := strings.Repeat("x", variables.MaxValueChars+1)
	if err := f.deps.Set(ctx, scope, "TOO_BIG", value, f.userID); !errors.Is(err, variables.ErrValueTooLong) {
		t.Errorf("Set long value: expected ErrValueTooLong, got %v", err)
	}
}

func TestSet_InvalidScopeRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, sc := range []variables.Scope{
		{},
		{RepoID: 1, OrgID: 2},
	} {
		if err := f.deps.Set(ctx, sc, "K", "v", 0); !errors.Is(err, variables.ErrInvalidScope) {
			t.Errorf("Set scope=%+v: expected ErrInvalidScope, got %v", sc, err)
		}
	}
}

func TestList_ReturnsValuesSortedAndMetadata(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	for _, item := range []struct {
		name  string
		value string
	}{
		{"BBB", "two"},
		{"AAA", "one"},
		{"ccc", "three"},
	} {
		if err := f.deps.Set(ctx, scope, item.name, item.value, f.userID); err != nil {
			t.Fatalf("Set %s: %v", item.name, err)
		}
	}
	got, err := f.deps.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d variables, want 3", len(got))
	}
	wantNames := []string{"AAA", "BBB", "ccc"}
	wantValues := []string{"one", "two", "three"}
	for i := range wantNames {
		if got[i].Name != wantNames[i] || got[i].Value != wantValues[i] {
			t.Errorf("got[%d] = %s/%q, want %s/%q", i, got[i].Name, got[i].Value, wantNames[i], wantValues[i])
		}
		if got[i].CreatedByUserID != f.userID {
			t.Errorf("got[%d].CreatedByUserID = %d, want %d", i, got[i].CreatedByUserID, f.userID)
		}
	}
}

func TestDelete_RemovesRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "TOKEN", "v", f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.deps.Delete(ctx, scope, "TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.deps.Get(ctx, scope, "TOKEN"); !errors.Is(err, variables.ErrNotFound) {
		t.Errorf("Get after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestDelete_MissingIsIdempotent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Delete(ctx, scope, "NEVER_EXISTED"); err != nil {
		t.Errorf("Delete missing: expected nil, got %v", err)
	}
}

func TestGet_CitextNameIsCaseInsensitive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	scope := variables.RepoScope(f.repoID)
	if err := f.deps.Set(ctx, scope, "IMAGE_TAG", "v1", f.userID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := f.deps.Get(ctx, scope, "image_tag")
	if err != nil {
		t.Fatalf("Get lowercased: %v", err)
	}
	if got.Value != "v1" {
		t.Errorf("got %q want v1", got.Value)
	}
}

func TestOrgAndRepoScopesAreIsolated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	repoScope := variables.RepoScope(f.repoID)
	orgScope := variables.OrgScope(f.orgID)
	if err := f.deps.Set(ctx, repoScope, "IMAGE_TAG", "repo", f.userID); err != nil {
		t.Fatalf("Set repo: %v", err)
	}
	if err := f.deps.Set(ctx, orgScope, "IMAGE_TAG", "org", f.userID); err != nil {
		t.Fatalf("Set org: %v", err)
	}
	repoVar, err := f.deps.Get(ctx, repoScope, "IMAGE_TAG")
	if err != nil {
		t.Fatalf("Get repo: %v", err)
	}
	orgVar, err := f.deps.Get(ctx, orgScope, "IMAGE_TAG")
	if err != nil {
		t.Fatalf("Get org: %v", err)
	}
	if repoVar.Value != "repo" || orgVar.Value != "org" {
		t.Fatalf("scope bleed: repo=%q org=%q", repoVar.Value, orgVar.Value)
	}
}
