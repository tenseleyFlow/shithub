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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestRepoDependencyUpdateConfigSync_StoresTeamOrgConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	org, repo := createDependencyUpdateOrgRepo(t, ctx, pool, rfs, "depupteam", true)
	gitDir, err := rfs.RepoPath(org.Slug, repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := initBareRepo(gitDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seedRepoFile(t, gitDir, ".github/dependabot.yml", `
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
      day: tuesday
      time: "10:15"
      timezone: America/New_York
    open-pull-requests-limit: 4
    groups:
      runtime:
        patterns: [ "example.com/*" ]
        applies-to: security-updates
  - package-ecosystem: npm
    directory: /frontend
    schedule:
      interval: daily
    vendor: true
`, "Add dependency update config")

	fixedNow := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	handler := jobs.RepoDependencyUpdateConfigSync(jobs.RepoDependencyUpdateConfigSyncDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixedNow },
	})
	payload, _ := json.Marshal(map[string]any{"repo_id": repo.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("RepoDependencyUpdateConfigSync: %v", err)
	}

	rq := reposdb.New()
	configs, err := rq.ListDependencyUpdateConfigsForRepo(ctx, pool, repo.ID)
	if err != nil {
		t.Fatalf("ListDependencyUpdateConfigsForRepo: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("len(configs) = %d, want 2", len(configs))
	}
	goCfg := configs[0]
	if goCfg.Ecosystem != "go" || goCfg.PackageManager != "gomod" || goCfg.Directory != "/" {
		t.Fatalf("go config = %+v", goCfg)
	}
	if goCfg.ScheduleInterval != "weekly" || goCfg.ScheduleDay != "tuesday" || goCfg.ScheduleTime != "10:15" || goCfg.ScheduleTimezone != "America/New_York" {
		t.Fatalf("go schedule = %+v", goCfg)
	}
	if goCfg.OpenPullRequestLimit != 4 || !goCfg.Enabled {
		t.Fatalf("go limit/enabled = %d/%v", goCfg.OpenPullRequestLimit, goCfg.Enabled)
	}
	if len(goCfg.RawConfigHash) != 64 || goCfg.LastSyncedSha == "" {
		t.Fatalf("hash/sha = %q/%q", goCfg.RawConfigHash, goCfg.LastSyncedSha)
	}
	wantNext := time.Date(2026, 5, 19, 14, 15, 0, 0, time.UTC)
	if !goCfg.NextRunAt.Valid || !goCfg.NextRunAt.Time.Equal(wantNext) {
		t.Fatalf("NextRunAt = %v, want %s", goCfg.NextRunAt, wantNext)
	}
	if string(goCfg.Groups) == "{}" {
		t.Fatalf("expected groups JSON, got %s", goCfg.Groups)
	}
	npmCfg := configs[1]
	if npmCfg.Ecosystem != "npm" || npmCfg.Directory != "/frontend" {
		t.Fatalf("npm config = %+v", npmCfg)
	}
	if !npmCfg.NextRunAt.Valid || !npmCfg.NextRunAt.Time.After(fixedNow) {
		t.Fatalf("npm NextRunAt = %v, want after %s", npmCfg.NextRunAt, fixedNow)
	}
	if len(npmCfg.UnsupportedKeys) != 1 || npmCfg.UnsupportedKeys[0] != "updates[1].vendor" {
		t.Fatalf("UnsupportedKeys = %#v", npmCfg.UnsupportedKeys)
	}

	jobsForRepo, err := rq.ListDependencyUpdateJobsForRepo(ctx, pool, reposdb.ListDependencyUpdateJobsForRepoParams{
		RepoID:    repo.ID,
		LimitRows: 5,
	})
	if err != nil {
		t.Fatalf("ListDependencyUpdateJobsForRepo: %v", err)
	}
	if len(jobsForRepo) != 1 || jobsForRepo[0].Status != "completed" {
		t.Fatalf("jobs = %#v", jobsForRepo)
	}
}

func TestRepoDependencyUpdateConfigSync_DisablesFreeOrgConfigs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	_, repo := createDependencyUpdateOrgRepo(t, ctx, pool, rfs, "depupfree", false)
	rq := reposdb.New()
	if _, err := rq.UpsertDependencyUpdateConfig(ctx, pool, reposdb.UpsertDependencyUpdateConfigParams{
		RepoID:               repo.ID,
		Ecosystem:            "go",
		PackageManager:       "gomod",
		Directory:            "/",
		ScheduleInterval:     "daily",
		OpenPullRequestLimit: 5,
		Enabled:              true,
		RawConfigHash:        "oldhash",
		RawConfigPath:        ".github/dependabot.yml",
		LastSyncedSha:        "oldsha",
		AllowRules:           []byte("[]"),
		IgnoreRules:          []byte("[]"),
		Groups:               []byte("{}"),
		Registries:           []byte("[]"),
	}); err != nil {
		t.Fatalf("UpsertDependencyUpdateConfig: %v", err)
	}

	handler := jobs.RepoDependencyUpdateConfigSync(jobs.RepoDependencyUpdateConfigSyncDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	payload, _ := json.Marshal(map[string]any{"repo_id": repo.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("RepoDependencyUpdateConfigSync: %v", err)
	}
	configs, err := rq.ListDependencyUpdateConfigsForRepo(ctx, pool, repo.ID)
	if err != nil {
		t.Fatalf("ListDependencyUpdateConfigsForRepo: %v", err)
	}
	if len(configs) != 1 || configs[0].Enabled {
		t.Fatalf("configs = %#v, want disabled existing config", configs)
	}
}

func createDependencyUpdateOrgRepo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rfs *storage.RepoFS, slug string, team bool) (orgsdb.Org, reposdb.Repo) {
	t.Helper()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: slug + "owner", DisplayName: "Dependency Update Owner", PasswordHash: secretScanFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	email, err := usersdb.New().CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID:    user.ID,
		Email:     slug + "owner@example.test",
		IsPrimary: true,
		Verified:  true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := usersdb.New().LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID:             user.ID,
		PrimaryEmailID: pgtype.Int8{Int64: email.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
	org, err := orgsdb.New().CreateOrg(ctx, pool, orgsdb.CreateOrgParams{
		Slug:            slug,
		DisplayName:     "Dependency Update Org",
		BillingEmail:    slug + "@example.test",
		CreatedByUserID: pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if team {
		now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
		if _, err := billing.ApplySubscriptionSnapshot(ctx, billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
			OrgID:                    org.ID,
			Plan:                     billing.PlanTeam,
			Status:                   billing.SubscriptionStatusActive,
			StripeSubscriptionID:     "sub_" + slug,
			StripeSubscriptionItemID: "si_" + slug,
			LicensedSeats:            1,
			CurrentPeriodStart:       now,
			CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
			LastWebhookEventID:       "evt_" + slug,
		}); err != nil {
			t.Fatalf("ApplySubscriptionSnapshot: %v", err)
		}
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "updates",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if _, err := rfs.RepoPath(org.Slug, repo.Name); err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	return org, repo
}
