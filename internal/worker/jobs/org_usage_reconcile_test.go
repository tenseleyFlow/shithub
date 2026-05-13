// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestOrgUsageReconcileEnqueuesActiveOrgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := createBillingUser(t, pool, "owner")
	firstID := createUsageReconcileOrg(t, pool, ownerID, "alpha")
	secondID := createUsageReconcileOrg(t, pool, ownerID, "beta")
	deletedID := createUsageReconcileOrg(t, pool, ownerID, "deleted")
	if err := orgs.SoftDelete(ctx, orgs.Deps{Pool: pool, Logger: discardLogger()}, deletedID, ownerID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	handler := jobs.OrgUsageReconcile(jobs.OrgUsageReconcileDeps{
		Pool:   pool,
		Logger: discardLogger(),
	})
	payload, _ := json.Marshal(jobs.OrgUsageReconcilePayload{
		Limit:        10,
		Source:       "scheduled-test",
		SkipSnapshot: true,
	})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("OrgUsageReconcile: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT payload FROM jobs WHERE kind = $1 ORDER BY id`, string(worker.KindOrgUsageRecalc))
	if err != nil {
		t.Fatalf("query queued recalc jobs: %v", err)
	}
	defer rows.Close()
	got := map[int64]jobs.OrgUsageRecalcPayload{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var p jobs.OrgUsageRecalcPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		got[p.OrgID] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("queued %d jobs, want 2: %#v", len(got), got)
	}
	for _, orgID := range []int64{firstID, secondID} {
		p, ok := got[orgID]
		if !ok {
			t.Fatalf("missing recalc job for active org %d; got %#v", orgID, got)
		}
		if p.Source != "scheduled-test" || !p.SkipSnapshot {
			t.Fatalf("payload for org %d = %#v", orgID, p)
		}
	}
	if _, ok := got[deletedID]; ok {
		t.Fatalf("soft-deleted org %d was enqueued: %#v", deletedID, got[deletedID])
	}
}

func TestOrgUsageReconcileRejectsNegativeLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	handler := jobs.OrgUsageReconcile(jobs.OrgUsageReconcileDeps{
		Pool:   pool,
		Logger: discardLogger(),
	})
	payload, _ := json.Marshal(jobs.OrgUsageReconcilePayload{Limit: -1})
	if err := handler(ctx, payload); !errors.Is(err, worker.ErrPoison) {
		t.Fatalf("OrgUsageReconcile error = %v, want poison", err)
	}
}

func createUsageReconcileOrg(t *testing.T, pool *pgxpool.Pool, ownerID int64, slug string) int64 {
	t.Helper()
	org, err := orgs.Create(context.Background(), orgs.Deps{Pool: pool, Logger: discardLogger()}, orgs.CreateParams{
		Slug:            slug,
		DisplayName:     slug,
		CreatedByUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("orgs.Create(%s): %v", slug, err)
	}
	return org.ID
}
