// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestRepoDependencyScan_StoresSupportedDependenciesAndAlerts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	owner, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "depowner", DisplayName: "Dependency Owner", PasswordHash: secretScanFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "deps",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	gitDir, err := rfs.RepoPath(owner.Username, repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := initBareRepo(gitDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seedRepoFile(t, gitDir, "go.mod", `module example.test/app

require example.test/vulnerable v1.2.3
`, "Add go.mod")

	advisory, err := rq.UpsertDependencyAdvisory(ctx, pool, reposdb.UpsertDependencyAdvisoryParams{
		Source:          "test-fixture",
		ExternalID:      "GHSA-test-vuln",
		Ecosystem:       "go",
		PackageName:     "example.test/primary",
		AffectedRange:   ">= v9.0.0",
		PatchedVersions: "v1.2.4",
		Severity:        "high",
		Summary:         "Fixture vulnerability",
		Description:     "Only used by the dependency scanner test.",
		ReferenceUrls:   []byte("[]"),
	})
	if err != nil {
		t.Fatalf("UpsertDependencyAdvisory: %v", err)
	}
	if err := rq.InsertDependencyAdvisoryAffectedRange(ctx, pool, reposdb.InsertDependencyAdvisoryAffectedRangeParams{
		AdvisoryID:      advisory.ID,
		Ecosystem:       "go",
		PackageName:     "example.test/vulnerable",
		RangeExpression: ">= v1.0.0, < v1.2.4",
		Metadata:        []byte("{}"),
	}); err != nil {
		t.Fatalf("InsertDependencyAdvisoryAffectedRange: %v", err)
	}

	handler := jobs.RepoDependencyScan(jobs.RepoDependencyScanDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	payload, _ := json.Marshal(map[string]any{"repo_id": repo.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("RepoDependencyScan: %v", err)
	}

	snap, err := rq.GetRepoDependencySnapshot(ctx, pool, repo.ID)
	if err != nil {
		t.Fatalf("GetRepoDependencySnapshot: %v", err)
	}
	if snap.ManifestCount != 1 || snap.DependencyCount != 1 {
		t.Fatalf("snapshot counts = %d/%d, want 1/1", snap.ManifestCount, snap.DependencyCount)
	}
	deps, err := rq.ListRepoDependenciesForRepo(ctx, pool, reposdb.ListRepoDependenciesForRepoParams{
		RepoID: repo.ID,
	})
	if err != nil {
		t.Fatalf("ListRepoDependenciesForRepo: %v", err)
	}
	if len(deps) != 1 || deps[0].PackageName != "example.test/vulnerable" || deps[0].PackageVersion != "v1.2.3" {
		t.Fatalf("deps = %#v, want vulnerable v1.2.3", deps)
	}
	alerts, err := rq.ListOpenDependencyAlertsForRepo(ctx, pool, repo.ID)
	if err != nil {
		t.Fatalf("ListOpenDependencyAlertsForRepo: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Severity != "high" || alerts[0].PatchedVersions != "v1.2.4" {
		t.Fatalf("alerts = %#v, want high fixture alert", alerts)
	}
}
