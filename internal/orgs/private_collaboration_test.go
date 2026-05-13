// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func TestOwnerExpansionRespectsPrivateCollaborationLimit(t *testing.T) {
	pool, deps, alice := setup(t)
	ctx := context.Background()
	org, err := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	repo := mustOrgRepo(t, pool, org.ID, "secret", "private")
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "bob"))
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "carol"))

	dave := mustUser(t, pool, "dave")
	if err := orgs.AddMember(ctx, deps, org.ID, dave, alice, "owner"); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("AddMember owner err=%v, want private collaboration limit", err)
	}
	if err := orgs.AddMember(ctx, deps, org.ID, dave, alice, "member"); err != nil {
		t.Fatalf("plain member add should not expand private collaboration: %v", err)
	}
	if err := orgs.ChangeRole(ctx, deps, org.ID, dave, "owner"); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("ChangeRole owner err=%v, want private collaboration limit", err)
	}
}

func TestTeamExpansionRespectsPrivateCollaborationLimit(t *testing.T) {
	pool, deps, alice := setup(t)
	ctx := context.Background()
	org, err := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	repo := mustOrgRepo(t, pool, org.ID, "secret", "private")
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "bob"))
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "carol"))

	team, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{OrgID: org.ID, Slug: "security", CreatedByUserID: alice})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := orgs.GrantTeamRepoAccess(ctx, deps, team.ID, repo.ID, alice, "read"); err != nil {
		t.Fatalf("empty-team private grant should not expand private collaboration: %v", err)
	}
	dave := mustUser(t, pool, "dave")
	if err := orgs.AddTeamMember(ctx, deps, team.ID, dave, alice, "member"); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("AddTeamMember err=%v, want private collaboration limit", err)
	}

	team2, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{OrgID: org.ID, Slug: "ops", CreatedByUserID: alice})
	if err != nil {
		t.Fatalf("create second team: %v", err)
	}
	insertTeamMemberRaw(t, pool, team2.ID, dave)
	if err := orgs.GrantTeamRepoAccess(ctx, deps, team2.ID, repo.ID, alice, "read"); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("GrantTeamRepoAccess err=%v, want private collaboration limit", err)
	}
	if err := orgs.RemoveTeamMember(ctx, deps, team2.ID, dave); err != nil {
		t.Fatalf("cleanup remove team member should remain allowed: %v", err)
	}
}

func TestOwnerInvitationsRespectPrivateCollaborationLimitAtSendAndAccept(t *testing.T) {
	pool, deps, alice := setup(t)
	ctx := context.Background()
	org, err := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	repo := mustOrgRepo(t, pool, org.ID, "secret", "private")
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "bob"))
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "carol"))

	dave := mustUser(t, pool, "dave")
	if _, err := orgs.Invite(ctx, deps, orgs.InviteParams{
		OrgID:           org.ID,
		InvitedByUserID: alice,
		TargetUsername:  "dave",
		Role:            "owner",
	}); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("Invite owner err=%v, want private collaboration limit", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM repo_collaborators WHERE repo_id = $1 AND user_id = $2`, repo.ID, dave); err != nil {
		t.Fatalf("cleanup accidental dave collab: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM repo_collaborators WHERE repo_id = $1 AND user_id = (SELECT id FROM users WHERE username = 'carol')`, repo.ID); err != nil {
		t.Fatalf("remove carol collab: %v", err)
	}
	res, err := orgs.Invite(ctx, deps, orgs.InviteParams{
		OrgID:           org.ID,
		InvitedByUserID: alice,
		TargetUsername:  "dave",
		Role:            "owner",
	})
	if err != nil {
		t.Fatalf("Invite owner under limit: %v", err)
	}
	insertDirectCollab(t, pool, repo.ID, mustUser(t, pool, "erin"))
	if err := orgs.AcceptInvitation(ctx, deps, res.Invitation, dave); !errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
		t.Fatalf("AcceptInvitation err=%v, want private collaboration limit", err)
	}
}

func mustOrgRepo(t *testing.T, db reposdb.DBTX, orgID int64, name, visibility string) reposdb.Repo {
	t.Helper()
	repo, err := reposdb.New().CreateRepo(context.Background(), db, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          name,
		Visibility:    reposdb.RepoVisibility(visibility),
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("create org repo %s: %v", name, err)
	}
	return repo
}

func insertDirectCollab(t *testing.T, db reposdb.DBTX, repoID, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO repo_collaborators (repo_id, user_id, role) VALUES ($1, $2, 'read')`, repoID, userID); err != nil {
		t.Fatalf("insert direct collaborator: %v", err)
	}
}

func insertTeamMemberRaw(t *testing.T, db reposdb.DBTX, teamID, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, teamID, userID); err != nil {
		t.Fatalf("insert team member: %v", err)
	}
}
