// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

func TestCreateEnqueuesBillingSeatSync(t *testing.T) {
	t.Parallel()
	pool, deps, alice := setup(t)

	org, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if got := countBillingSeatSyncJobs(t, pool, org.ID); got != 1 {
		t.Fatalf("billing seat sync jobs=%d, want 1", got)
	}
}

func TestMemberChangesEnqueueBillingSeatSync(t *testing.T) {
	t.Parallel()
	pool, deps, alice := setup(t)
	bob := mustUser(t, pool, "bob")

	org, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgs.AddMember(context.Background(), deps, org.ID, bob, alice, "member"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if got := countBillingSeatSyncJobs(t, pool, org.ID); got != 2 {
		t.Fatalf("billing seat sync jobs after add=%d, want 2", got)
	}
	if err := orgs.RemoveMember(context.Background(), deps, org.ID, bob); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if got := countBillingSeatSyncJobs(t, pool, org.ID); got != 3 {
		t.Fatalf("billing seat sync jobs after remove=%d, want 3", got)
	}
}

func TestAcceptInvitationEnqueuesBillingSeatSync(t *testing.T) {
	t.Parallel()
	pool, deps, alice := setup(t)
	bob := mustUser(t, pool, "bob")

	org, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	res, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: org.ID, InvitedByUserID: alice,
		TargetUsername: "bob", Role: "member",
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if err := orgs.AcceptInvitation(context.Background(), deps, res.Invitation, bob); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if got := countBillingSeatSyncJobs(t, pool, org.ID); got != 2 {
		t.Fatalf("billing seat sync jobs after accept=%d, want 2", got)
	}
}

func countBillingSeatSyncJobs(t *testing.T, pool *pgxpool.Pool, orgID int64) int {
	t.Helper()
	var jobs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'org_id' = $2`,
		worker.KindOrgBillingSeatSync, strconv.FormatInt(orgID, 10),
	).Scan(&jobs); err != nil {
		t.Fatalf("query billing seat sync jobs: %v", err)
	}
	return jobs
}
