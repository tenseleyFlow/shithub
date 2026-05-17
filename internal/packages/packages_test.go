// SPDX-License-Identifier: AGPL-3.0-or-later

package packages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/packages"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func setup(t *testing.T) (*pgxpool.Pool, usersdb.User, orgsdb.Org, reposdb.Repo) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)

	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("Create org: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "pkg-repo",
		Description:   "packages",
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return pool, user, org, repo
}

func TestPublishFileCountsTowardOrgPackageStorage(t *testing.T) {
	pool, user, org, repo := setup(t)
	ctx := context.Background()
	deps := packages.Deps{Pool: pool}
	objectKey, err := packages.NewObjectKey(repo.ID, "cli-tool", "v1.0.0", "cli-tool.tar.gz")
	if err != nil {
		t.Fatalf("NewObjectKey: %v", err)
	}

	res, err := packages.PublishFile(ctx, deps, packages.PublishInput{
		RepoID:      repo.ID,
		Name:        "cli-tool",
		Version:     "v1.0.0",
		Description: "A generic package",
		Filename:    "cli-tool.tar.gz",
		ObjectKey:   objectKey,
		ContentType: "application/gzip",
		SizeBytes:   1234,
		ETag:        "etag-test",
		ActorUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("PublishFile: %v", err)
	}
	if res.Package.PackageBytes != 1234 {
		t.Fatalf("package bytes = %d, want 1234", res.Package.PackageBytes)
	}
	if res.Version.SizeBytes != 1234 {
		t.Fatalf("version bytes = %d, want 1234", res.Version.SizeBytes)
	}

	list, err := packages.ListRepoPackages(ctx, deps, repo.ID)
	if err != nil {
		t.Fatalf("ListRepoPackages: %v", err)
	}
	if len(list) != 1 || list[0].Name != "cli-tool" || list[0].PackageBytes != 1234 || list[0].VersionCount != 1 {
		t.Fatalf("package list mismatch: %+v", list)
	}

	start, end := billing.MonthlyUsagePeriod(time.Now())
	counters, err := billing.RecalculateOrgUsageCounters(ctx, billing.Deps{Pool: pool}, org.ID, start, end)
	if err != nil {
		t.Fatalf("RecalculateOrgUsageCounters: %v", err)
	}
	if counters.PackageStorageBytes != 1234 {
		t.Fatalf("package storage = %d, want 1234", counters.PackageStorageBytes)
	}
	if counters.ObjectStorageBytes != 1234 {
		t.Fatalf("object storage = %d, want package bytes included", counters.ObjectStorageBytes)
	}

	keys, err := packages.DeleteRepoPackage(ctx, deps, repo.ID, res.Package.ID)
	if err != nil {
		t.Fatalf("DeleteRepoPackage: %v", err)
	}
	if len(keys) != 1 || keys[0] != objectKey {
		t.Fatalf("deleted keys = %#v, want %q", keys, objectKey)
	}
	counters, err = billing.RecalculateOrgUsageCounters(ctx, billing.Deps{Pool: pool}, org.ID, start, end)
	if err != nil {
		t.Fatalf("RecalculateOrgUsageCounters after delete: %v", err)
	}
	if counters.PackageStorageBytes != 0 || counters.ObjectStorageBytes != 0 {
		t.Fatalf("storage after delete = package:%d object:%d, want zero", counters.PackageStorageBytes, counters.ObjectStorageBytes)
	}
}

func TestPublishFileRejectsDuplicateVersionFilename(t *testing.T) {
	pool, user, _, repo := setup(t)
	ctx := context.Background()
	deps := packages.Deps{Pool: pool}
	objectKey, err := packages.NewObjectKey(repo.ID, "cli-tool", "v1.0.0", "cli-tool.tar.gz")
	if err != nil {
		t.Fatalf("NewObjectKey: %v", err)
	}
	input := packages.PublishInput{
		RepoID:      repo.ID,
		Name:        "cli-tool",
		Version:     "v1.0.0",
		Filename:    "cli-tool.tar.gz",
		ObjectKey:   objectKey,
		SizeBytes:   12,
		ActorUserID: user.ID,
	}
	if _, err := packages.PublishFile(ctx, deps, input); err != nil {
		t.Fatalf("PublishFile first: %v", err)
	}
	input.ObjectKey, err = packages.NewObjectKey(repo.ID, "cli-tool", "v1.0.0", "cli-tool.tar.gz")
	if err != nil {
		t.Fatalf("NewObjectKey second: %v", err)
	}
	if _, err := packages.PublishFile(ctx, deps, input); !errors.Is(err, packages.ErrPackageFileExists) {
		t.Fatalf("PublishFile duplicate err=%v, want ErrPackageFileExists", err)
	}
}
