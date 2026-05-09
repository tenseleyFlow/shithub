// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestOrgOwner_ImplicitAdmin pins the S30 contract: an `org_members.role
// = 'owner'` row promotes the user to RoleAdmin on every repo owned by
// that org. Without this, an org owner can't push to their own org's
// repos (the dogfood-blocking case).
func TestOrgOwner_ImplicitAdmin(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	creator, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	deps := orgs.Deps{Pool: pool}
	org, err := orgs.Create(ctx, deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme", CreatedByUserID: creator.ID,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	// Org-owned private repo.
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Valid: false},
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "secret",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create org repo: %v", err)
	}
	ref := policy.NewRepoRefFromRepo(repo)

	actor := policy.UserActor(creator.ID, "alice", false, false)
	pdeps := policy.Deps{Pool: pool}

	// Push (write tier) must allow.
	if got := policy.Can(ctx, pdeps, actor, policy.ActionRepoWrite, ref); !got.Allow {
		t.Fatalf("org owner should be able to write: %+v", got)
	}
	// Repo-admin tier must allow.
	if got := policy.Can(ctx, pdeps, actor, policy.ActionRepoAdmin, ref); !got.Allow {
		t.Fatalf("org owner should be admin: %+v", got)
	}

	// A non-member must NOT see a private org repo.
	bob, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	bobActor := policy.UserActor(bob.ID, "bob", false, false)
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoRead, ref); got.Allow {
		t.Fatalf("non-member should not read private org repo: %+v", got)
	}

	// Member-but-not-owner must NOT get implicit admin (teams are S31).
	if err := orgs.AddMember(ctx, deps, org.ID, bob.ID, creator.ID, "member"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoWrite, ref); got.Allow {
		t.Fatalf("plain org member should NOT have implicit write: %+v", got)
	}
	// And read on a private org repo for a plain member is also denied
	// today — implicit read for org members is deferred to S31 (teams).
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoRead, ref); got.Allow {
		t.Fatalf("plain org member should NOT have implicit read on private repo: %+v", got)
	}
}
