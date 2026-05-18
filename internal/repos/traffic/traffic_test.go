// SPDX-License-Identifier: AGPL-3.0-or-later

package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestRecordViewAndCloneAggregatesUniqueVisitors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID := createTrafficRepo(t, pool)
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)

	event := Event{
		RepoID:       repoID,
		OccurredAt:   now,
		VisitorKey:   "anon:203.0.113.10|TestUA",
		Path:         "/blob/trunk/README.md?plain=1",
		ReferrerHost: "GitHub.COM:443",
	}
	for i := 0; i < 2; i++ {
		if err := RecordView(ctx, pool, event); err != nil {
			t.Fatalf("RecordView same visitor: %v", err)
		}
	}
	event.VisitorKey = "user:42"
	if err := RecordView(ctx, pool, event); err != nil {
		t.Fatalf("RecordView second visitor: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := RecordClone(ctx, pool, Event{RepoID: repoID, OccurredAt: now, VisitorKey: "user:42"}); err != nil {
			t.Fatalf("RecordClone: %v", err)
		}
	}

	summary, err := LoadSummary(ctx, pool, repoID, now)
	if err != nil {
		t.Fatalf("LoadSummary: %v", err)
	}
	if summary.TotalViews != 3 || summary.UniqueViews != 2 {
		t.Fatalf("views totals = %d/%d, want 3/2", summary.TotalViews, summary.UniqueViews)
	}
	if summary.TotalClones != 2 || summary.UniqueClones != 1 {
		t.Fatalf("clone totals = %d/%d, want 2/1", summary.TotalClones, summary.UniqueClones)
	}
	if len(summary.TopPaths) != 1 || summary.TopPaths[0].Name != "/blob/trunk/README.md" ||
		summary.TopPaths[0].Views != 3 || summary.TopPaths[0].UniqueViews != 2 {
		t.Fatalf("top paths = %#v, want README 3/2", summary.TopPaths)
	}
	if len(summary.TopReferrers) != 1 || summary.TopReferrers[0].Name != "github.com" ||
		summary.TopReferrers[0].Views != 3 || summary.TopReferrers[0].UniqueViews != 2 {
		t.Fatalf("top referrers = %#v, want github.com 3/2", summary.TopReferrers)
	}

	var leaked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM repo_traffic_uniques
		 WHERE repo_id = $1 AND (key LIKE '%203.0.113.10%' OR key LIKE '%TestUA%')`,
		repoID).Scan(&leaked); err != nil {
		t.Fatalf("privacy check: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("traffic unique rows leaked raw visitor data")
	}
}

func TestExternalReferrerHost(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://github.com/owner/repo":         "github.com",
		"https://github.com:443/owner/repo":     "github.com",
		"https://shithub.example/alice/project": "",
		"not a url":                             "",
		"":                                      "",
	}
	for raw, want := range tests {
		if got := ExternalReferrerHost(raw, "shithub.example"); got != want {
			t.Fatalf("ExternalReferrerHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func createTrafficRepo(t *testing.T, db reposdb.DBTX) int64 {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, db, usersdb.CreateUserParams{
		Username:     "traffic-owner",
		DisplayName:  "Traffic Owner",
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, db, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "traffic-repo",
		Description:   "",
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID
}
