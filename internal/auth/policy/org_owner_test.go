// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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

// TestTeamGrant_GivesWriteAccess pins the S31 contract: a team grant
// at `write` on an org repo lets that team's members push, even
// though they're plain org members (not owners) and have no direct
// collaborator row.
func TestTeamGrant_GivesWriteAccess(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	creator, _ := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	deps := orgs.Deps{Pool: pool}
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: creator.ID})
	repo, _ := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
		Name:       "demo", DefaultBranch: "trunk", Visibility: reposdb.RepoVisibilityPublic,
	})
	bob, _ := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err := orgs.AddMember(ctx, deps, org.ID, bob.ID, creator.ID, "member"); err != nil {
		t.Fatalf("add org member: %v", err)
	}
	team, _ := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "eng", CreatedByUserID: creator.ID,
	})
	if err := orgs.AddTeamMember(ctx, deps, team.ID, bob.ID, creator.ID, "member"); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := orgs.GrantTeamRepoAccess(ctx, deps, team.ID, repo.ID, creator.ID, "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	ref := policy.NewRepoRefFromRepo(repo)
	bobActor := policy.UserActor(bob.ID, "bob", false, false)
	pdeps := policy.Deps{Pool: pool}

	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoWrite, ref); !got.Allow {
		t.Fatalf("team-granted write should allow: %+v", got)
	}

	// Demote to read → write must now deny.
	if err := orgs.GrantTeamRepoAccess(ctx, deps, team.ID, repo.ID, creator.ID, "read"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoWrite, ref); got.Allow {
		t.Fatal("after demote to read, write should deny")
	}
	// Reads still allow under read role.
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoRead, ref); !got.Allow {
		t.Fatalf("read should still allow at read role: %+v", got)
	}

	// Revoke entirely → reads on a public repo still allow (visibility),
	// but for the test of "team membership lost access" we use a
	// private repo branch:
	if _, err := pool.Exec(ctx, `UPDATE repos SET visibility='private' WHERE id=$1`, repo.ID); err != nil {
		t.Fatalf("flip private: %v", err)
	}
	// Cache from the previous Can() call may still be hot — start a
	// fresh request scope by using a new context.
	if err := orgs.RevokeTeamRepoAccess(ctx, deps, team.ID, repo.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	freshCtx := context.Background()
	if got := policy.Can(freshCtx, pdeps, bobActor, policy.ActionRepoRead,
		policy.NewRepoRefFromRepo(reposdbReload(t, pool, repo.ID))); got.Allow {
		t.Fatalf("after revoke, private read should deny: %+v", got)
	}
}

// TestTeamParent_Inheritance pins the one-level parent inheritance
// rule: a child-team member inherits the parent team's repo grants.
func TestTeamParent_Inheritance(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	creator, _ := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	deps := orgs.Deps{Pool: pool}
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: creator.ID})
	repo, _ := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
		Name:       "demo", DefaultBranch: "trunk", Visibility: reposdb.RepoVisibilityPublic,
	})
	bob, _ := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	_ = orgs.AddMember(ctx, deps, org.ID, bob.ID, creator.ID, "member")

	parent, _ := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "engineering", CreatedByUserID: creator.ID,
	})
	child, _ := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "eng-mobile", ParentTeamID: parent.ID,
		CreatedByUserID: creator.ID,
	})
	// Bob is in the CHILD team only. Grant write on the PARENT team.
	_ = orgs.AddTeamMember(ctx, deps, child.ID, bob.ID, creator.ID, "member")
	if err := orgs.GrantTeamRepoAccess(ctx, deps, parent.ID, repo.ID, creator.ID, "write"); err != nil {
		t.Fatalf("parent grant: %v", err)
	}

	ref := policy.NewRepoRefFromRepo(repo)
	bobActor := policy.UserActor(bob.ID, "bob", false, false)
	pdeps := policy.Deps{Pool: pool}
	if got := policy.Can(ctx, pdeps, bobActor, policy.ActionRepoWrite, ref); !got.Allow {
		t.Fatalf("child team should inherit parent's write grant: %+v", got)
	}
}

// reposdbReload re-fetches a repo row so a follow-up policy.Can sees
// fresh visibility after a raw UPDATE.
func reposdbReload(t *testing.T, pool *pgxpool.Pool, id int64) reposdb.Repo {
	t.Helper()
	row, err := reposdb.New().GetRepoByID(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("reload repo: %v", err)
	}
	return row
}

// TestOrgSuspended_BlocksWrites pins the S30 contract: when an org
// is suspended, every write action against an org-owned repo is
// denied — even for the org owner. Reads still allow.
func TestOrgSuspended_BlocksWrites(t *testing.T) {
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
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Valid: false},
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	// Suspend the org via raw UPDATE (mirrors what the admin
	// surface will eventually do).
	if _, err := pool.Exec(ctx, `UPDATE orgs SET suspended_at=now() WHERE id=$1`, org.ID); err != nil {
		t.Fatalf("suspend org: %v", err)
	}

	ref := policy.NewRepoRefFromRepo(repo)
	actor := policy.UserActor(creator.ID, "alice", false, false)
	pdeps := policy.Deps{Pool: pool}

	// Reads still allowed.
	if got := policy.Can(ctx, pdeps, actor, policy.ActionRepoRead, ref); !got.Allow {
		t.Fatalf("reads on suspended-org repo should allow: %+v", got)
	}
	// Writes denied with the typed code.
	got := policy.Can(ctx, pdeps, actor, policy.ActionRepoWrite, ref)
	if got.Allow {
		t.Fatal("write on suspended-org repo should deny")
	}
	if got.Code != policy.DenyOrgSuspended {
		t.Fatalf("want DenyOrgSuspended code, got %v (reason=%q)", got.Code, got.Reason)
	}
}
