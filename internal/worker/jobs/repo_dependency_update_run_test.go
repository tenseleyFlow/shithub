// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

type fakeDependencyVersionResolver map[string]string

func (f fakeDependencyVersionResolver) LatestVersion(_ context.Context, ecosystem, packageName, currentVersion string) (string, error) {
	if v, ok := f[ecosystem+"|"+packageName]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no fake latest version for %s %s %s", ecosystem, packageName, currentVersion)
}

type dependencyUpdateRunFixture struct {
	pool   *pgxpool.Pool
	rfs    *storage.RepoFS
	repo   reposdb.Repo
	gitDir string
}

func TestRepoDependencyUpdateRunCreatesGroupedGoVersionPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupDependencyUpdateRunFixture(t, ctx, "depupgroup", `module example.test/app

require (
	example.test/alpha v1.2.3
	example.test/beta v0.4.0
)
`)
	cfg := upsertDependencyUpdateRunConfig(t, ctx, env.pool, env.repo.ID, 5, []byte(`{"runtime":{"patterns":["example.test/*"],"applies_to":"version-updates"}}`))
	job := createDependencyUpdateRunJob(t, ctx, env.pool, env.repo.ID, cfg.ID, "version_update")

	runDependencyUpdateJob(t, ctx, env, job.ID, fakeDependencyVersionResolver{
		"go|example.test/alpha": "v1.2.4",
		"go|example.test/beta":  "v0.5.0",
	})

	gotJob := getDependencyUpdateJob(t, ctx, env.pool, job.ID)
	if gotJob.Status != "completed" {
		t.Fatalf("job status = %s, want completed: %s", gotJob.Status, gotJob.LastError)
	}
	summary := decodeDependencyUpdateRunSummary(t, gotJob.ResultSummary)
	if summary["pull_request_count"] != float64(1) {
		t.Fatalf("summary pull_request_count = %#v, want 1; summary=%#v", summary["pull_request_count"], summary)
	}

	prs := listDependencyUpdatePRs(t, ctx, env.pool, env.repo.ID)
	if len(prs) != 1 {
		t.Fatalf("len(update PRs) = %d, want 1", len(prs))
	}
	if prs[0].UpdateKind != "grouped" || prs[0].Status != "open" {
		t.Fatalf("dependency update PR = %+v, want grouped/open", prs[0])
	}
	var packageSet []map[string]any
	if err := json.Unmarshal(prs[0].PackageSet, &packageSet); err != nil {
		t.Fatalf("decode package set: %v", err)
	}
	if len(packageSet) != 2 {
		t.Fatalf("package set len = %d, want 2: %#v", len(packageSet), packageSet)
	}
	updated := readRepoBlob(t, ctx, env.gitDir, prs[0].BranchName, "go.mod")
	for _, want := range []string{"example.test/alpha v1.2.4", "example.test/beta v0.5.0"} {
		if !strings.Contains(updated, want) {
			t.Fatalf("updated go.mod missing %q:\n%s", want, updated)
		}
	}

	// Replaying the same worker payload after completion should be a no-op.
	runDependencyUpdateJob(t, ctx, env, job.ID, fakeDependencyVersionResolver{})
	if got := listDependencyUpdatePRs(t, ctx, env.pool, env.repo.ID); len(got) != 1 {
		t.Fatalf("replayed job created extra PRs: %#v", got)
	}
}

func TestRepoDependencyUpdateRunCreatesSecurityGoPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupDependencyUpdateRunFixture(t, ctx, "depupsecure", `module example.test/app

require example.test/vulnerable v1.2.3
`)
	cfg := upsertDependencyUpdateRunConfig(t, ctx, env.pool, env.repo.ID, 5, []byte("{}"))
	rq := reposdb.New()
	if _, err := rq.UpsertDependencyAdvisory(ctx, env.pool, reposdb.UpsertDependencyAdvisoryParams{
		Source:          "test-fixture",
		ExternalID:      "GHSA-depup-secure",
		Ecosystem:       "go",
		PackageName:     "example.test/vulnerable",
		AffectedRange:   "v1.2.3",
		PatchedVersions: "v1.2.4",
		Severity:        "high",
		Summary:         "Fixture vulnerable dependency",
		Description:     "Only used by the dependency update run test.",
		ReferenceUrls:   []byte("[]"),
	}); err != nil {
		t.Fatalf("UpsertDependencyAdvisory: %v", err)
	}

	scan := jobs.RepoDependencyScan(jobs.RepoDependencyScanDeps{
		Pool:   env.pool,
		RepoFS: env.rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	scanPayload, _ := json.Marshal(map[string]any{"repo_id": env.repo.ID})
	if err := scan(ctx, scanPayload); err != nil {
		t.Fatalf("RepoDependencyScan: %v", err)
	}
	jobsForRepo, err := rq.ListDependencyUpdateJobsForRepo(ctx, env.pool, reposdb.ListDependencyUpdateJobsForRepoParams{
		RepoID:    env.repo.ID,
		LimitRows: 5,
	})
	if err != nil {
		t.Fatalf("ListDependencyUpdateJobsForRepo: %v", err)
	}
	var job reposdb.DependencyUpdateJob
	for _, candidate := range jobsForRepo {
		if candidate.ConfigID.Valid && candidate.ConfigID.Int64 == cfg.ID && candidate.JobKind == "security_update" {
			job = candidate
			break
		}
	}
	if job.ID == 0 || job.Status != "queued" || job.TriggerSource != "dependency_scan" {
		t.Fatalf("security update job = %+v, jobs=%#v", job, jobsForRepo)
	}

	runDependencyUpdateJob(t, ctx, env, job.ID, fakeDependencyVersionResolver{})

	prs := listDependencyUpdatePRs(t, ctx, env.pool, env.repo.ID)
	if len(prs) != 1 {
		t.Fatalf("len(update PRs) = %d, want 1", len(prs))
	}
	if prs[0].UpdateKind != "security" {
		t.Fatalf("update kind = %s, want security", prs[0].UpdateKind)
	}
	updated := readRepoBlob(t, ctx, env.gitDir, prs[0].BranchName, "go.mod")
	if !strings.Contains(updated, "example.test/vulnerable v1.2.4") {
		t.Fatalf("updated go.mod missing patched version:\n%s", updated)
	}
}

func TestRepoDependencyUpdateRunRespectsOpenPullRequestLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupDependencyUpdateRunFixture(t, ctx, "depuplimit", `module example.test/app

require example.test/alpha v1.2.3
`)
	cfg := upsertDependencyUpdateRunConfig(t, ctx, env.pool, env.repo.ID, 0, []byte("{}"))
	job := createDependencyUpdateRunJob(t, ctx, env.pool, env.repo.ID, cfg.ID, "version_update")

	runDependencyUpdateJob(t, ctx, env, job.ID, fakeDependencyVersionResolver{
		"go|example.test/alpha": "v1.2.4",
	})

	if prs := listDependencyUpdatePRs(t, ctx, env.pool, env.repo.ID); len(prs) != 0 {
		t.Fatalf("update PRs = %#v, want none", prs)
	}
	gotJob := getDependencyUpdateJob(t, ctx, env.pool, job.ID)
	summary := decodeDependencyUpdateRunSummary(t, gotJob.ResultSummary)
	if got := summary["message"]; got != "open dependency update pull request limit reached" {
		t.Fatalf("summary message = %#v, want open PR limit message; summary=%#v", got, summary)
	}
}

func setupDependencyUpdateRunFixture(t *testing.T, ctx context.Context, slug, goMod string) dependencyUpdateRunFixture {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	org, repo := createDependencyUpdateOrgRepo(t, ctx, pool, rfs, slug, true)
	gitDir, err := rfs.RepoPath(org.Slug, repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := initBareRepo(gitDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seedRepoFile(t, gitDir, "go.mod", goMod, "Add go.mod")
	return dependencyUpdateRunFixture{
		pool:   pool,
		rfs:    rfs,
		repo:   repo,
		gitDir: gitDir,
	}
}

func upsertDependencyUpdateRunConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64, openPRLimit int32, groups []byte) reposdb.DependencyUpdateConfig {
	t.Helper()
	cfg, err := reposdb.New().UpsertDependencyUpdateConfig(ctx, pool, reposdb.UpsertDependencyUpdateConfigParams{
		RepoID:               repoID,
		Ecosystem:            "go",
		PackageManager:       "gomod",
		Directory:            "/",
		ScheduleInterval:     "weekly",
		ScheduleDay:          "monday",
		ScheduleTime:         "09:00",
		OpenPullRequestLimit: openPRLimit,
		Enabled:              true,
		RawConfigHash:        "runhash",
		RawConfigPath:        ".github/dependabot.yml",
		LastSyncedSha:        "abc123",
		AllowRules:           []byte("[]"),
		IgnoreRules:          []byte("[]"),
		Groups:               groups,
		Registries:           []byte("[]"),
		NextRunAt:            pgtype.Timestamptz{Time: time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC), Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertDependencyUpdateConfig: %v", err)
	}
	return cfg
}

func createDependencyUpdateRunJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID, cfgID int64, kind string) reposdb.DependencyUpdateJob {
	t.Helper()
	job, err := reposdb.New().CreateDependencyUpdateJob(ctx, pool, reposdb.CreateDependencyUpdateJobParams{
		RepoID:        repoID,
		ConfigID:      pgtype.Int8{Int64: cfgID, Valid: true},
		JobKind:       kind,
		Status:        "queued",
		TriggerSource: "test",
		ResultSummary: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateDependencyUpdateJob: %v", err)
	}
	return job
}

func runDependencyUpdateJob(t *testing.T, ctx context.Context, env dependencyUpdateRunFixture, jobID int64, resolver fakeDependencyVersionResolver) {
	t.Helper()
	handler := jobs.RepoDependencyUpdateRun(jobs.RepoDependencyUpdateRunDeps{
		Pool:            env.pool,
		RepoFS:          env.rfs,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		VersionResolver: resolver,
		Now:             func() time.Time { return time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC) },
	})
	payload, _ := json.Marshal(jobs.RepoDependencyUpdateRunPayload{JobID: jobID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("RepoDependencyUpdateRun: %v", err)
	}
}

func getDependencyUpdateJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64) reposdb.DependencyUpdateJob {
	t.Helper()
	job, err := reposdb.New().GetDependencyUpdateJob(ctx, pool, jobID)
	if err != nil {
		t.Fatalf("GetDependencyUpdateJob: %v", err)
	}
	return job
}

func decodeDependencyUpdateRunSummary(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode run summary: %v\n%s", err, string(body))
	}
	return out
}

func listDependencyUpdatePRs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repoID int64) []reposdb.DependencyUpdatePr {
	t.Helper()
	prs, err := reposdb.New().ListDependencyUpdatePRsForRepo(ctx, pool, repoID)
	if err != nil {
		t.Fatalf("ListDependencyUpdatePRsForRepo: %v", err)
	}
	return prs
}

func readRepoBlob(t *testing.T, ctx context.Context, gitDir, ref, path string) string {
	t.Helper()
	body, err := repogit.ReadBlobBytes(ctx, gitDir, ref, path, 1<<20)
	if err != nil {
		t.Fatalf("ReadBlobBytes %s:%s: %v", ref, path, err)
	}
	return string(body)
}
