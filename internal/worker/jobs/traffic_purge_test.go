// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

const trafficPurgeFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// The retention cutoff is `day < today - RetentionDays`, so a row landing
// exactly on the cutoff day survives. Seed one row on each side of it plus
// one well inside the window, and assert only the older-than-cutoff row goes.
func TestTrafficPurgeKeepsTheRetentionWindow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID := insertTrafficPurgeRepo(t, ctx, pool, "traffic-purge-window")

	today := startOfUTCDay(time.Now())
	inWindow := today.AddDate(0, 0, -5)
	onCutoff := today.AddDate(0, 0, -repotraffic.DefaultRetentionDays)
	pastCutoff := today.AddDate(0, 0, -repotraffic.DefaultRetentionDays-1)
	// repo_traffic_daily keeps a much longer window of its own.
	dailyInWindow := today.AddDate(0, 0, -100)
	dailyPastCutoff := today.AddDate(0, 0, -repotraffic.DefaultDailyRetentionDays-1)

	for _, day := range []time.Time{inWindow, onCutoff, pastCutoff} {
		insertTrafficPath(t, ctx, pool, repoID, day, "/README.md")
		insertTrafficReferrer(t, ctx, pool, repoID, day, "example.com")
		insertTrafficUnique(t, ctx, pool, repoID, day, "view", "", 0x01)
	}
	for _, day := range []time.Time{inWindow, dailyInWindow, dailyPastCutoff} {
		insertTrafficDaily(t, ctx, pool, repoID, day)
	}

	handler := jobs.TrafficPurge(jobs.TrafficPurgeDeps{Pool: pool})
	if err := handler(ctx, nil); err != nil {
		t.Fatalf("traffic:purge handler: %v", err)
	}

	assertTrafficDays(t, ctx, pool, "repo_traffic_paths", []time.Time{onCutoff, inWindow})
	assertTrafficDays(t, ctx, pool, "repo_traffic_referrers", []time.Time{onCutoff, inWindow})
	assertTrafficDays(t, ctx, pool, "repo_traffic_uniques", []time.Time{onCutoff, inWindow})
	assertTrafficDays(t, ctx, pool, "repo_traffic_daily", []time.Time{dailyInWindow, inWindow})

	// Idempotent: a second run over the already-trimmed tables is a no-op.
	if err := handler(ctx, nil); err != nil {
		t.Fatalf("traffic:purge second run: %v", err)
	}
	assertTrafficDays(t, ctx, pool, "repo_traffic_paths", []time.Time{onCutoff, inWindow})
	assertTrafficDays(t, ctx, pool, "repo_traffic_daily", []time.Time{dailyInWindow, inWindow})
}

// A single run must not delete more than BatchSize × MaxBatches rows per
// table, so a first pass over a multi-million-row backlog stays bounded.
func TestTrafficPurgeBoundsDeletesPerRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	repoID := insertTrafficPurgeRepo(t, ctx, pool, "traffic-purge-bounded")

	today := startOfUTCDay(time.Now())
	old := today.AddDate(0, 0, -90)
	const seeded = 25
	for i := range seeded {
		insertTrafficPath(t, ctx, pool, repoID, old, fmt.Sprintf("/blob/%d", i))
	}

	res, err := repotraffic.Purge(ctx, pool, repotraffic.PurgeOptions{
		BatchSize:  4,
		MaxBatches: 3,
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.PathsDeleted != 12 {
		t.Fatalf("PathsDeleted = %d, want 12 (3 batches of 4)", res.PathsDeleted)
	}
	if !res.Remaining {
		t.Fatal("Remaining = false, want true: the run stopped on the batch cap")
	}
	if got := countTrafficRows(t, ctx, pool, "repo_traffic_paths"); got != seeded-12 {
		t.Fatalf("rows left = %d, want %d", got, seeded-12)
	}

	// The next pass picks up where this one stopped; the job is safe to
	// re-run until the backlog drains.
	res, err = repotraffic.Purge(ctx, pool, repotraffic.PurgeOptions{
		BatchSize:  4,
		MaxBatches: 3,
	})
	if err != nil {
		t.Fatalf("Purge second run: %v", err)
	}
	if res.PathsDeleted != 12 {
		t.Fatalf("second run PathsDeleted = %d, want 12", res.PathsDeleted)
	}
	if got := countTrafficRows(t, ctx, pool, "repo_traffic_paths"); got != 1 {
		t.Fatalf("rows left after two runs = %d, want 1", got)
	}
}

func startOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func insertTrafficPurgeRepo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     name,
		DisplayName:  name,
		PasswordHash: trafficPurgeFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          name,
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID
}

func insertTrafficPath(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64, day time.Time, path string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO repo_traffic_paths (repo_id, day, path, views, unique_views, created_at)
VALUES ($1, $2::date, $3, 1, 1, $4)`, repoID, day.UTC().Format(time.DateOnly), path, day.UTC()); err != nil {
		t.Fatalf("insert repo_traffic_paths(%s, %s): %v", day.Format(time.DateOnly), path, err)
	}
}

func insertTrafficReferrer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64, day time.Time, referrer string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO repo_traffic_referrers (repo_id, day, referrer, views, unique_views, created_at)
VALUES ($1, $2::date, $3, 1, 1, $4)`, repoID, day.UTC().Format(time.DateOnly), referrer, day.UTC()); err != nil {
		t.Fatalf("insert repo_traffic_referrers(%s): %v", day.Format(time.DateOnly), err)
	}
}

// created_at matters here: repo_traffic_uniques is purged on created_at,
// which the request path stamps on the same day it derives `day` from.
func insertTrafficUnique(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64, day time.Time, metric, key string, seed byte) {
	t.Helper()
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = seed
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO repo_traffic_uniques (repo_id, day, metric, key, visitor_hash, created_at)
VALUES ($1, $2::date, $3, $4, $5, $6)`, repoID, day.UTC().Format(time.DateOnly), metric, key, hash, day.UTC()); err != nil {
		t.Fatalf("insert repo_traffic_uniques(%s): %v", day.Format(time.DateOnly), err)
	}
}

func insertTrafficDaily(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64, day time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO repo_traffic_daily (repo_id, day, views, unique_views, clones, unique_clones, created_at)
VALUES ($1, $2::date, 1, 1, 0, 0, $3)`, repoID, day.UTC().Format(time.DateOnly), day.UTC()); err != nil {
		t.Fatalf("insert repo_traffic_daily(%s): %v", day.Format(time.DateOnly), err)
	}
}

// assertTrafficDays checks the surviving rows of a table are exactly the
// supplied days, ordered ascending.
func assertTrafficDays(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, want []time.Time) {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT day FROM "+table+" ORDER BY day ASC")
	if err != nil {
		t.Fatalf("select %s: %v", table, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var day pgtype.Date
		if err := rows.Scan(&day); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		got = append(got, day.Time.UTC().Format(time.DateOnly))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	wantDays := make([]string, 0, len(want))
	for _, d := range want {
		wantDays = append(wantDays, d.UTC().Format(time.DateOnly))
	}
	if len(got) != len(wantDays) {
		t.Fatalf("%s survivors = %v, want %v", table, got, wantDays)
	}
	for i := range got {
		if got[i] != wantDays[i] {
			t.Fatalf("%s survivors = %v, want %v", table, got, wantDays)
		}
	}
}

func countTrafficRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
