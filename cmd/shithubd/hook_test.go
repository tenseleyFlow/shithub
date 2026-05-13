// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestEnforcePreReceiveStorageQuotaRejectsOrgRepoOverLimit(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: "$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // #nosec G101 -- test fixture password hash, not a credential
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	gitDir, err := rfs.RepoPath(org.Slug, repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o750); err != nil {
		t.Fatalf("mkdir git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "objects", "pack"), []byte("0123456789"), 0o640); err != nil {
		t.Fatalf("write repo bytes: %v", err)
	}
	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: pool}, billing.QuotaOverrideInput{
		OrgID:           org.ID,
		Kind:            billing.QuotaKindStorageBytes,
		LimitValue:      5,
		CreatedByUserID: user.ID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride: %v", err)
	}

	h := &hookCtx{pool: pool}
	if err := enforcePreReceiveStorageQuota(ctx, h, repo, rfs, gitDir, []refUpdate{{
		before: strings.Repeat("1", 40),
		after:  strings.Repeat("0", 40),
		ref:    "refs/heads/old",
	}}); err != nil {
		t.Fatalf("delete-only quota enforce: %v", err)
	}
	err = enforcePreReceiveStorageQuota(ctx, h, repo, rfs, gitDir, []refUpdate{{
		before: strings.Repeat("1", 40),
		after:  strings.Repeat("2", 40),
		ref:    "refs/heads/trunk",
	}})
	if !errors.Is(err, errHookStorage) {
		t.Fatalf("enforcePreReceiveStorageQuota err = %v, want errHookStorage", err)
	}

	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: pool}, billing.QuotaOverrideInput{
		OrgID:           org.ID,
		Kind:            billing.QuotaKindStorageBytes,
		Unlimited:       true,
		CreatedByUserID: user.ID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride unlimited: %v", err)
	}
	if err := enforcePreReceiveStorageQuota(ctx, h, repo, rfs, gitDir, []refUpdate{{
		before: strings.Repeat("1", 40),
		after:  strings.Repeat("2", 40),
		ref:    "refs/heads/trunk",
	}}); err != nil {
		t.Fatalf("unlimited quota enforce: %v", err)
	}
}
