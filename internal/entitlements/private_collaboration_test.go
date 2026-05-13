// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestPrivateCollaborationUsageCountsEffectivePrivateAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	owner := createEntitlementUser(t, pool, "owner")
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{Slug: "acme", CreatedByUserID: owner.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	privateRepo := createEntitlementOrgRepo(t, pool, org.ID, "secret", "private")
	publicRepo := createEntitlementOrgRepo(t, pool, org.ID, "public", "public")

	plainMember := createEntitlementUser(t, pool, "plain")
	insertOrgMember(t, pool, org.ID, plainMember.ID, "member")

	direct := createEntitlementUser(t, pool, "direct")
	insertRepoCollaborator(t, pool, privateRepo.ID, direct.ID)
	publicOnly := createEntitlementUser(t, pool, "publiconly")
	insertRepoCollaborator(t, pool, publicRepo.ID, publicOnly.ID)

	parentTeamID := insertEntitlementTeam(t, pool, org.ID, "platform", 0)
	childTeamID := insertEntitlementTeam(t, pool, org.ID, "runtime", parentTeamID)
	childMember := createEntitlementUser(t, pool, "childmember")
	insertTeamMember(t, pool, childTeamID, childMember.ID)
	insertTeamRepoGrant(t, pool, parentTeamID, privateRepo.ID)

	usage, err := entitlements.PrivateCollaborationUsageForOrg(ctx, entitlements.Deps{Pool: pool}, org.ID)
	if err != nil {
		t.Fatalf("PrivateCollaborationUsageForOrg: %v", err)
	}
	if usage.Count != 3 {
		t.Fatalf("private collaborator count=%d, want owner + direct + inherited team member", usage.Count)
	}
	if usage.Limit != entitlements.FreePrivateCollaborationLimit || usage.Unlimited {
		t.Fatalf("free usage limit = %+v", usage)
	}
}

func TestPrivateCollaborationExpansionEnforcesFreeLimitAndTeamUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	owner := createEntitlementUser(t, pool, "owner")
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{Slug: "acme", CreatedByUserID: owner.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	createEntitlementOrgRepo(t, pool, org.ID, "secret", "private")
	first := createEntitlementUser(t, pool, "first")
	second := createEntitlementUser(t, pool, "second")
	third := createEntitlementUser(t, pool, "third")

	check, err := entitlements.CheckPrivateCollaborationExpansion(ctx, entitlements.Deps{Pool: pool}, org.ID, entitlements.PrivateCollaborationExpansion{
		CandidateUserIDs: []int64{first.ID, second.ID},
	})
	if err != nil {
		t.Fatalf("allowed expansion: %v", err)
	}
	if !check.Allowed || check.WouldUse != 3 {
		t.Fatalf("two-user free expansion check = %+v, want allowed at limit", check)
	}

	check, err = entitlements.CheckPrivateCollaborationExpansion(ctx, entitlements.Deps{Pool: pool}, org.ID, entitlements.PrivateCollaborationExpansion{
		CandidateUserIDs: []int64{first.ID, second.ID, third.ID},
	})
	if err != nil {
		t.Fatalf("blocked expansion: %v", err)
	}
	if check.Allowed || check.WouldUse != 4 || check.Err() != entitlements.ErrPrivateCollaborationLimitExceeded {
		t.Fatalf("three-user free expansion check = %+v, want blocked", check)
	}
	if !strings.Contains(check.Message(), "up to 3 private collaborators") {
		t.Fatalf("message=%q, want concrete limit", check.Message())
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := setSubscription(ctx, billing.Deps{Pool: pool}, org.ID, now, billing.PlanTeam, billing.SubscriptionStatusActive, "private-collab"); err != nil {
		t.Fatalf("activate team: %v", err)
	}
	check, err = entitlements.CheckPrivateCollaborationExpansion(ctx, entitlements.Deps{Pool: pool, Now: func() time.Time { return now }}, org.ID, entitlements.PrivateCollaborationExpansion{
		CandidateUserIDs: []int64{first.ID, second.ID, third.ID},
	})
	if err != nil {
		t.Fatalf("team expansion: %v", err)
	}
	if !check.Allowed || !check.Usage.Unlimited {
		t.Fatalf("team expansion check = %+v, want unlimited", check)
	}
}

func TestPrivateRepoCreationCountsOwnersForFirstPrivateRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	owner := createEntitlementUser(t, pool, "owner")
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{Slug: "acme", CreatedByUserID: owner.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for _, name := range []string{"owner2", "owner3", "owner4"} {
		insertOrgMember(t, pool, org.ID, createEntitlementUser(t, pool, name).ID, "owner")
	}

	check, err := entitlements.CheckPrivateRepositoryCreation(ctx, entitlements.Deps{Pool: pool}, org.ID)
	if err != nil {
		t.Fatalf("CheckPrivateRepositoryCreation: %v", err)
	}
	if check.Allowed || check.WouldUse != 4 {
		t.Fatalf("first-private-repo check = %+v, want blocked by four owners", check)
	}
}

func TestRepoPrivateVisibilityCountsRepoSpecificGrants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	owner := createEntitlementUser(t, pool, "owner")
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{Slug: "acme", CreatedByUserID: owner.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	repo := createEntitlementOrgRepo(t, pool, org.ID, "soon-private", "public")
	insertRepoCollaborator(t, pool, repo.ID, createEntitlementUser(t, pool, "direct").ID)
	teamID := insertEntitlementTeam(t, pool, org.ID, "security", 0)
	insertTeamMember(t, pool, teamID, createEntitlementUser(t, pool, "teamuser").ID)
	insertTeamRepoGrant(t, pool, teamID, repo.ID)

	check, err := entitlements.CheckRepoPrivateVisibility(ctx, entitlements.Deps{Pool: pool}, org.ID, repo.ID)
	if err != nil {
		t.Fatalf("CheckRepoPrivateVisibility: %v", err)
	}
	if !check.Allowed || check.WouldUse != 3 {
		t.Fatalf("public-to-private check = %+v, want owner + direct + team user allowed at limit", check)
	}

	insertRepoCollaborator(t, pool, repo.ID, createEntitlementUser(t, pool, "extra").ID)
	check, err = entitlements.CheckRepoPrivateVisibility(ctx, entitlements.Deps{Pool: pool}, org.ID, repo.ID)
	if err != nil {
		t.Fatalf("CheckRepoPrivateVisibility after extra: %v", err)
	}
	if check.Allowed || check.WouldUse != 4 {
		t.Fatalf("public-to-private check with extra = %+v, want blocked", check)
	}
}

func createEntitlementUser(t *testing.T, db usersdb.DBTX, username string) usersdb.User {
	t.Helper()
	user, err := usersdb.New().CreateUser(context.Background(), db, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  username,
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return user
}

func createEntitlementOrgRepo(t *testing.T, db reposdb.DBTX, orgID int64, name, visibility string) reposdb.Repo {
	t.Helper()
	repo, err := reposdb.New().CreateRepo(context.Background(), db, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          name,
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibility(visibility),
	})
	if err != nil {
		t.Fatalf("create repo %s: %v", name, err)
	}
	return repo
}

func insertOrgMember(t *testing.T, db orgsdbtx, orgID, userID int64, role string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)`, orgID, userID, role); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
}

func insertRepoCollaborator(t *testing.T, db orgsdbtx, repoID, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO repo_collaborators (repo_id, user_id, role) VALUES ($1, $2, 'read')`, repoID, userID); err != nil {
		t.Fatalf("insert repo collaborator: %v", err)
	}
}

func insertEntitlementTeam(t *testing.T, db orgsdbtx, orgID int64, slug string, parentTeamID int64) int64 {
	t.Helper()
	var id int64
	if parentTeamID == 0 {
		if err := db.QueryRow(context.Background(), `INSERT INTO teams (org_id, slug, display_name) VALUES ($1, $2, $2) RETURNING id`, orgID, slug).Scan(&id); err != nil {
			t.Fatalf("insert team: %v", err)
		}
		return id
	}
	if err := db.QueryRow(context.Background(), `INSERT INTO teams (org_id, slug, display_name, parent_team_id) VALUES ($1, $2, $2, $3) RETURNING id`, orgID, slug, parentTeamID).Scan(&id); err != nil {
		t.Fatalf("insert child team: %v", err)
	}
	return id
}

func insertTeamMember(t *testing.T, db orgsdbtx, teamID, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, teamID, userID); err != nil {
		t.Fatalf("insert team member: %v", err)
	}
}

func insertTeamRepoGrant(t *testing.T, db orgsdbtx, teamID, repoID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO team_repo_access (team_id, repo_id, role) VALUES ($1, $2, 'read')`, teamID, repoID); err != nil {
		t.Fatalf("insert team repo grant: %v", err)
	}
}

type orgsdbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
