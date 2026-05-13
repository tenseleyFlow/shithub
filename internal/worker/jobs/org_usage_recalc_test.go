// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestOrgUsageRecalcUpdatesCountersAndSnapshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupOrgBillingSeatSync(t)
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          "metered",
		Description:   "metered repo",
		Visibility:    reposdb.RepoVisibilityPrivate,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := rq.UpdateRepoDiskUsed(ctx, pool, reposdb.UpdateRepoDiskUsedParams{
		ID:            repo.ID,
		DiskUsedBytes: 123456,
	}); err != nil {
		t.Fatalf("UpdateRepoDiskUsed: %v", err)
	}

	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	handler := jobs.OrgUsageRecalc(jobs.OrgUsageRecalcDeps{
		Pool:   pool,
		Logger: discardLogger(),
		Now:    func() time.Time { return now },
	})
	payload, _ := json.Marshal(jobs.OrgUsageRecalcPayload{OrgID: orgID, Source: "worker-test"})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("OrgUsageRecalc: %v", err)
	}

	counters, err := billing.GetOrgUsageCounters(ctx, billing.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgUsageCounters: %v", err)
	}
	if counters.RepoStorageBytes != 123456 {
		t.Fatalf("repo storage bytes = %d, want 123456", counters.RepoStorageBytes)
	}
	wantStart, wantEnd := billing.MonthlyUsagePeriod(now)
	if !counters.ActionsPeriodStart.Valid || !counters.ActionsPeriodStart.Time.Equal(wantStart) ||
		!counters.ActionsPeriodEnd.Valid || !counters.ActionsPeriodEnd.Time.Equal(wantEnd) {
		t.Fatalf("period = %v/%v, want %s/%s", counters.ActionsPeriodStart, counters.ActionsPeriodEnd, wantStart, wantEnd)
	}

	snapshots, err := billing.ListOrgUsageSnapshots(ctx, billing.Deps{Pool: pool}, orgID, 1)
	if err != nil {
		t.Fatalf("ListOrgUsageSnapshots: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Source != "worker-test" || snapshots[0].RepoStorageBytes != 123456 {
		t.Fatalf("snapshot = %+v", snapshots)
	}
}
