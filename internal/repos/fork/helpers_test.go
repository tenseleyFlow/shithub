// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const ownerSlugFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// helpersDeps builds a Deps with a real pool but a null logger.
// Repo-FS isn't used by ownerSlug so we omit it.
func helpersDeps(t *testing.T) (Deps, context.Context) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	return Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, context.Background()
}

func TestOwnerSlug_UserOwnedReturnsUsername(t *testing.T) {
	t.Parallel()
	deps, ctx := helpersDeps(t)
	u, err := usersdb.New().CreateUser(ctx, deps.Pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: ownerSlugFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, deps.Pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: u.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	got, err := ownerSlug(ctx, deps, repo)
	if err != nil {
		t.Fatalf("ownerSlug: %v", err)
	}
	if got != "alice" {
		t.Errorf("got %q, want %q", got, "alice")
	}
}

func TestOwnerSlug_OrgOwnedReturnsOrgSlug(t *testing.T) {
	t.Parallel()
	deps, ctx := helpersDeps(t)
	// Creator user — orgs require a created_by_user_id.
	u, err := usersdb.New().CreateUser(ctx, deps.Pool, usersdb.CreateUserParams{
		Username: "founder", DisplayName: "Founder", PasswordHash: ownerSlugFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgsdb.New().CreateOrg(ctx, deps.Pool, orgsdb.CreateOrgParams{
		Slug: "fortrangoingonforty", DisplayName: "Fortran Going on Forty",
		Description: "", BillingEmail: "billing@example.com",
		CreatedByUserID: pgtype.Int8{Int64: u.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, deps.Pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "ferp",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo (org-owned): %v", err)
	}
	got, err := ownerSlug(ctx, deps, repo)
	if err != nil {
		t.Fatalf("ownerSlug (org-owned): %v", err)
	}
	if got != "fortrangoingonforty" {
		t.Errorf("got %q, want %q", got, "fortrangoingonforty")
	}
}

func TestOwnerSlug_NoOwnerErrors(t *testing.T) {
	t.Parallel()
	deps, ctx := helpersDeps(t)
	// Synthesize a repo with neither owner set. We don't insert it —
	// just probe the resolver's fall-through branch.
	repo := reposdb.Repo{ID: 999_999}
	_, err := ownerSlug(ctx, deps, repo)
	if err == nil {
		t.Fatal("ownerSlug unexpectedly succeeded for ownerless repo")
	}
	if !strings.Contains(err.Error(), "neither user nor org owner") {
		t.Errorf("error should name the missing-owner condition: %v", err)
	}
}
