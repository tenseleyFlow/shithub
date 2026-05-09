// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/orgs"
)

func TestCreateTeam_HappyPath(t *testing.T) {
	_, deps, alice := setup(t)
	ctx := context.Background()
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	team, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "engineering", DisplayName: "Engineering",
		CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if string(team.Slug) != "engineering" {
		t.Fatalf("slug: want engineering, got %q", team.Slug)
	}
}

func TestCreateTeam_RejectsInvalidSlug(t *testing.T) {
	_, deps, alice := setup(t)
	ctx := context.Background()
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	_, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "BAD SLUG", CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrTeamSlugInvalid) {
		t.Fatalf("want ErrTeamSlugInvalid, got %v", err)
	}
}

func TestCreateTeam_RejectsDuplicateSlug(t *testing.T) {
	_, deps, alice := setup(t)
	ctx := context.Background()
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	if _, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "eng", CreatedByUserID: alice,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "eng", CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrTeamSlugTaken) {
		t.Fatalf("want ErrTeamSlugTaken, got %v", err)
	}
}

// One-level-deep nesting: creating a team with a parent that already
// has a parent must be refused at the trigger.
func TestCreateTeam_RejectsTwoLevelNesting(t *testing.T) {
	_, deps, alice := setup(t)
	ctx := context.Background()
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	gp, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "g", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	parent, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "p", ParentTeamID: gp.ID, CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	_, err = orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "c", ParentTeamID: parent.ID, CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrTeamNestingTooDeep) {
		t.Fatalf("want ErrTeamNestingTooDeep, got %v", err)
	}
}

func TestSetTeamParent_RejectsSelfParent(t *testing.T) {
	_, deps, alice := setup(t)
	ctx := context.Background()
	org, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	team, _ := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org.ID, Slug: "eng", CreatedByUserID: alice,
	})
	err := orgs.SetTeamParent(ctx, deps, team.ID, team.ID)
	if !errors.Is(err, orgs.ErrTeamSelfParent) {
		t.Fatalf("want ErrTeamSelfParent, got %v", err)
	}
}

func TestCreateTeam_RejectsCrossOrgParent(t *testing.T) {
	pool, deps, alice := setup(t)
	ctx := context.Background()
	bob := mustUser(t, pool, "bob")
	org1, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "acme", CreatedByUserID: alice})
	org2, _ := orgs.Create(ctx, deps, orgs.CreateParams{Slug: "beta", CreatedByUserID: bob})
	parent, _ := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org2.ID, Slug: "g", CreatedByUserID: bob,
	})
	_, err := orgs.CreateTeam(ctx, deps, orgs.CreateTeamParams{
		OrgID: org1.ID, Slug: "c", ParentTeamID: parent.ID, CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrTeamCrossOrgParent) {
		t.Fatalf("want ErrTeamCrossOrgParent, got %v", err)
	}
}
