// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestRepoDependencyUpdateSweepCreatesVersionJobAndAdvancesConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	_, repo := createDependencyUpdateOrgRepo(t, ctx, pool, rfs, "depupsweep", true)
	rq := reposdb.New()
	dueAt := time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC)
	cfg, err := rq.UpsertDependencyUpdateConfig(ctx, pool, reposdb.UpsertDependencyUpdateConfigParams{
		RepoID:               repo.ID,
		Ecosystem:            "go",
		PackageManager:       "gomod",
		Directory:            "/",
		ScheduleInterval:     "weekly",
		ScheduleDay:          "monday",
		ScheduleTime:         "09:00",
		OpenPullRequestLimit: 5,
		Enabled:              true,
		RawConfigHash:        "sweephash",
		RawConfigPath:        ".github/dependabot.yml",
		LastSyncedSha:        "abc123",
		AllowRules:           []byte("[]"),
		IgnoreRules:          []byte("[]"),
		Groups:               []byte("{}"),
		Registries:           []byte("[]"),
		NextRunAt:            pgtype.Timestamptz{Time: dueAt, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertDependencyUpdateConfig: %v", err)
	}

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	handler := jobs.RepoDependencyUpdateSweep(jobs.RepoDependencyUpdateSweepDeps{
		Pool:      pool,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return now },
		BatchSize: 10,
	})
	if err := handler(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RepoDependencyUpdateSweep: %v", err)
	}

	jobsForRepo, err := rq.ListDependencyUpdateJobsForRepo(ctx, pool, reposdb.ListDependencyUpdateJobsForRepoParams{
		RepoID:    repo.ID,
		LimitRows: 5,
	})
	if err != nil {
		t.Fatalf("ListDependencyUpdateJobsForRepo: %v", err)
	}
	if len(jobsForRepo) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobsForRepo))
	}
	job := jobsForRepo[0]
	if job.ConfigID.Int64 != cfg.ID || !job.ConfigID.Valid || job.JobKind != "version_update" || job.Status != "queued" {
		t.Fatalf("job = %+v", job)
	}
	if !job.ScheduledFor.Valid || !job.ScheduledFor.Time.Equal(dueAt) {
		t.Fatalf("ScheduledFor = %v, want %s", job.ScheduledFor, dueAt)
	}

	got, err := rq.GetDependencyUpdateConfig(ctx, pool, cfg.ID)
	if err != nil {
		t.Fatalf("GetDependencyUpdateConfig: %v", err)
	}
	wantNext := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !got.NextRunAt.Valid || !got.NextRunAt.Time.Equal(wantNext) {
		t.Fatalf("NextRunAt = %v, want %s", got.NextRunAt, wantNext)
	}
	if !got.LastCheckedAt.Valid {
		t.Fatalf("LastCheckedAt not set")
	}
}

func TestRepoDependencyUpdateSweepIgnoresFutureConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	_, repo := createDependencyUpdateOrgRepo(t, ctx, pool, rfs, "depupfuture", true)
	rq := reposdb.New()
	future := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	if _, err := rq.UpsertDependencyUpdateConfig(ctx, pool, reposdb.UpsertDependencyUpdateConfigParams{
		RepoID:               repo.ID,
		Ecosystem:            "npm",
		PackageManager:       "npm",
		Directory:            "/",
		ScheduleInterval:     "daily",
		ScheduleTime:         "09:00",
		OpenPullRequestLimit: 5,
		Enabled:              true,
		RawConfigHash:        "futurehash",
		RawConfigPath:        ".github/dependabot.yml",
		LastSyncedSha:        "abc123",
		AllowRules:           []byte("[]"),
		IgnoreRules:          []byte("[]"),
		Groups:               []byte("{}"),
		Registries:           []byte("[]"),
		NextRunAt:            pgtype.Timestamptz{Time: future, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertDependencyUpdateConfig: %v", err)
	}

	handler := jobs.RepoDependencyUpdateSweep(jobs.RepoDependencyUpdateSweepDeps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC) },
	})
	if err := handler(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RepoDependencyUpdateSweep: %v", err)
	}
	jobsForRepo, err := rq.ListDependencyUpdateJobsForRepo(ctx, pool, reposdb.ListDependencyUpdateJobsForRepoParams{
		RepoID:    repo.ID,
		LimitRows: 5,
	})
	if err != nil {
		t.Fatalf("ListDependencyUpdateJobsForRepo: %v", err)
	}
	if len(jobsForRepo) != 0 {
		t.Fatalf("jobs = %#v, want none", jobsForRepo)
	}
}
