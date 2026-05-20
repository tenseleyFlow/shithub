// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
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

func TestEnforcePreReceiveSecretProtectionRejectsPublicRepoSecret(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo := createHookUserRepo(t, pool, reposdb.RepoVisibilityPublic)
	gitDir, commit := danglingSecretCommit(t)

	err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}})
	var secretErr errHookSecretProtection
	if !errors.As(err, &secretErr) {
		t.Fatalf("enforcePreReceiveSecretProtection err = %v, want errHookSecretProtection", err)
	}
	if len(secretErr.Findings) != 1 {
		t.Fatalf("findings len = %d, want 1", len(secretErr.Findings))
	}
	msg := friendlyHookErr(err)
	for _, want := range []string{"config/secrets.env:1", "github-token"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("friendly message missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ") {
		t.Fatalf("friendly message leaked raw secret: %s", msg)
	}
	var status secretscandb.SecretScanBypassStatus
	var pattern, path, commitOID string
	var lineNo int
	if err := pool.QueryRow(ctx, `SELECT status, pattern, path, commit_oid, line_no FROM secret_scan_bypass_requests WHERE repo_id = $1`, repo.ID).
		Scan(&status, &pattern, &path, &commitOID, &lineNo); err != nil {
		t.Fatalf("lookup bypass request: %v", err)
	}
	if status != secretscandb.SecretScanBypassStatusPending || pattern != "github-token" || path != "config/secrets.env" || commitOID != commit || lineNo != 1 {
		t.Fatalf("bypass request = (%s, %s, %s, %s, %d), want pending github-token config/secrets.env %s 1",
			status, pattern, path, commitOID, lineNo, commit)
	}
}

func TestEnforcePreReceiveSecretProtectionHonorsAllowlist(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo := createHookUserRepo(t, pool, reposdb.RepoVisibilityPublic)
	if _, err := secretscandb.New().InsertSecretScanAllowlist(ctx, pool, secretscandb.InsertSecretScanAllowlistParams{
		RepoID:    repo.ID,
		Pattern:   "github-token",
		Path:      "config/secrets.env",
		Reason:    "fixture false positive",
		CreatedBy: pgtype.Int8{Int64: user.ID, Valid: true},
	}); err != nil {
		t.Fatalf("InsertSecretScanAllowlist: %v", err)
	}
	gitDir, commit := danglingSecretCommit(t)

	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}); err != nil {
		t.Fatalf("enforcePreReceiveSecretProtection allowlisted err = %v", err)
	}
}

func TestEnforcePreReceiveSecretProtectionHonorsApprovedBypass(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo := createHookUserRepo(t, pool, reposdb.RepoVisibilityPublic)
	gitDir, commit := danglingSecretCommit(t)
	seedSecretBypassRequest(t, pool, repo.ID, user.ID, commit, secretscandb.SecretScanBypassStatusApproved, time.Now().UTC().Add(24*time.Hour))

	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}); err != nil {
		t.Fatalf("enforcePreReceiveSecretProtection approved bypass err = %v", err)
	}
}

func TestEnforcePreReceiveSecretProtectionDeniedBypassStillRejects(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo := createHookUserRepo(t, pool, reposdb.RepoVisibilityPublic)
	gitDir, commit := danglingSecretCommit(t)
	seedSecretBypassRequest(t, pool, repo.ID, user.ID, commit, secretscandb.SecretScanBypassStatusDenied, time.Time{})

	var secretErr errHookSecretProtection
	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}); !errors.As(err, &secretErr) {
		t.Fatalf("denied bypass err = %v, want errHookSecretProtection", err)
	}
}

func TestEnforcePreReceiveSecretProtectionExpiredBypassReturnsToPending(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo := createHookUserRepo(t, pool, reposdb.RepoVisibilityPublic)
	gitDir, commit := danglingSecretCommit(t)
	row := seedSecretBypassRequest(t, pool, repo.ID, user.ID, commit, secretscandb.SecretScanBypassStatusApproved, time.Now().UTC().Add(-time.Hour))

	var secretErr errHookSecretProtection
	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}); !errors.As(err, &secretErr) {
		t.Fatalf("expired bypass err = %v, want errHookSecretProtection", err)
	}
	refreshed, err := secretscandb.New().GetSecretScanBypassRequest(ctx, pool, secretscandb.GetSecretScanBypassRequestParams{ID: row.ID, RepoID: repo.ID})
	if err != nil {
		t.Fatalf("GetSecretScanBypassRequest: %v", err)
	}
	if refreshed.Status != secretscandb.SecretScanBypassStatusPending || refreshed.ReviewedAt.Valid || refreshed.ApprovedUntil.Valid {
		t.Fatalf("expired bypass status = %s reviewed=%v approved_until=%v, want pending unreviewed",
			refreshed.Status, refreshed.ReviewedAt.Valid, refreshed.ApprovedUntil.Valid)
	}
}

func TestEnforcePreReceiveSecretProtectionPrivateOrgRequiresTeam(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo, orgID := createHookOrgRepo(t, pool, reposdb.RepoVisibilityPrivate)
	gitDir, commit := danglingSecretCommit(t)
	refs := []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}

	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, refs); err != nil {
		t.Fatalf("free private org should skip push protection, got err = %v", err)
	}
	if _, err := billing.ApplySubscriptionSnapshot(ctx, billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_hook_sec",
		StripeSubscriptionItemID: "si_hook_sec",
		LastWebhookEventID:       "evt_hook_sec",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	var secretErr errHookSecretProtection
	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, refs); !errors.As(err, &secretErr) {
		t.Fatalf("team private org err = %v, want errHookSecretProtection", err)
	}
}

func TestEnforcePreReceiveSecretProtectionPrivateTeamOrgCustomPattern(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	user, repo, orgID := createHookOrgRepo(t, pool, reposdb.RepoVisibilityPrivate)
	if _, err := secretscandb.New().CreateSecretScanCustomPattern(ctx, pool, secretscandb.CreateSecretScanCustomPatternParams{
		OrgID:       orgID,
		Name:        "internal-token",
		Description: "Internal token fixture.",
		Pattern:     `shithub_custom_[A-Za-z0-9]{12,}`,
		MinMatchLen: 16,
		CreatedBy:   pgtype.Int8{Int64: user.ID, Valid: true},
	}); err != nil {
		t.Fatalf("CreateSecretScanCustomPattern: %v", err)
	}
	gitDir, commit := danglingCustomSecretCommit(t)
	refs := []refUpdate{{
		before: strings.Repeat("0", 40),
		after:  commit,
		ref:    "refs/heads/trunk",
	}}

	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, refs); err != nil {
		t.Fatalf("free private org should skip custom push protection, got err = %v", err)
	}
	if _, err := billing.ApplySubscriptionSnapshot(ctx, billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_hook_custom_sec",
		StripeSubscriptionItemID: "si_hook_custom_sec",
		LastWebhookEventID:       "evt_hook_custom_sec",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	var secretErr errHookSecretProtection
	if err := enforcePreReceiveSecretProtection(ctx, &hookCtx{pool: pool, userID: user.ID}, repo, gitDir, refs); !errors.As(err, &secretErr) {
		t.Fatalf("team private org custom err = %v, want errHookSecretProtection", err)
	}
	if len(secretErr.Findings) != 1 {
		t.Fatalf("findings len = %d, want 1", len(secretErr.Findings))
	}
	if secretErr.Findings[0].Pattern != "custom/internal-token" {
		t.Fatalf("Pattern = %q, want custom/internal-token", secretErr.Findings[0].Pattern)
	}
	if msg := friendlyHookErr(secretErr); strings.Contains(msg, "shithub_custom_ABCDEF123456") {
		t.Fatalf("friendly message leaked raw custom secret: %s", msg)
	}
}

func createHookUserRepo(t *testing.T, pool *pgxpool.Pool, visibility reposdb.RepoVisibility) (usersdb.User, reposdb.Repo) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: "$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // #nosec G101 -- test fixture password hash, not a credential
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    visibility,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return user, repo
}

func createHookOrgRepo(t *testing.T, pool *pgxpool.Pool, visibility reposdb.RepoVisibility) (usersdb.User, reposdb.Repo, int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "org-owner",
		DisplayName:  "Org Owner",
		PasswordHash: "$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // #nosec G101 -- test fixture password hash, not a credential
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, orgs.CreateParams{
		Slug: "hook-acme", DisplayName: "Hook Acme", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    visibility,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return user, repo, org.ID
}

func danglingSecretCommit(t *testing.T) (string, string) {
	return danglingCommitWithBody(t, "config/secrets.env", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ\n")
}

func danglingCustomSecretCommit(t *testing.T) (string, string) {
	return danglingCommitWithBody(t, "config/internal.env", "TOKEN=shithub_custom_ABCDEF123456\n")
}

func danglingCommitWithBody(t *testing.T, path, body string) (string, string) {
	t.Helper()
	ctx := context.Background()
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	gitDir, err := rfs.RepoPath("hook", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := rfs.InitBare(ctx, gitDir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	commit, err := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Message:     "Add config",
		Branch:      "scan-tmp",
		Files: []repogit.FileEntry{{
			Path: path,
			Body: []byte(body),
		}},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	if out, err := exec.Command("git", "-C", gitDir, "update-ref", "-d", "refs/heads/scan-tmp").CombinedOutput(); err != nil {
		t.Fatalf("delete temp ref: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return gitDir, commit
}

func seedSecretBypassRequest(t *testing.T, pool *pgxpool.Pool, repoID, userID int64, commit string, status secretscandb.SecretScanBypassStatus, approvedUntil time.Time) secretscandb.SecretScanBypassRequest {
	t.Helper()
	ctx := context.Background()
	row, err := secretscandb.New().UpsertSecretScanBypassRequest(ctx, pool, secretscandb.UpsertSecretScanBypassRequestParams{
		RepoID:        repoID,
		Pattern:       "github-token",
		Path:          "config/secrets.env",
		CommitOid:     commit,
		LineNo:        1,
		RequestedBy:   pgtype.Int8{Int64: userID, Valid: true},
		RequestReason: "test fixture",
	})
	if err != nil {
		t.Fatalf("UpsertSecretScanBypassRequest: %v", err)
	}
	switch status {
	case secretscandb.SecretScanBypassStatusPending:
		return row
	case secretscandb.SecretScanBypassStatusApproved:
		row, err = secretscandb.New().ReviewSecretScanBypassRequest(ctx, pool, secretscandb.ReviewSecretScanBypassRequestParams{
			ID:            row.ID,
			RepoID:        repoID,
			Status:        secretscandb.SecretScanBypassStatusApproved,
			ReviewedBy:    pgtype.Int8{Int64: userID, Valid: true},
			ReviewNote:    "approved fixture",
			ApprovedUntil: pgtype.Timestamptz{Time: approvedUntil, Valid: true},
		})
	case secretscandb.SecretScanBypassStatusDenied:
		row, err = secretscandb.New().ReviewSecretScanBypassRequest(ctx, pool, secretscandb.ReviewSecretScanBypassRequestParams{
			ID:            row.ID,
			RepoID:        repoID,
			Status:        secretscandb.SecretScanBypassStatusDenied,
			ReviewedBy:    pgtype.Int8{Int64: userID, Valid: true},
			ReviewNote:    "denied fixture",
			ApprovedUntil: pgtype.Timestamptz{},
		})
	default:
		t.Fatalf("unsupported bypass status %q", status)
	}
	if err != nil {
		t.Fatalf("ReviewSecretScanBypassRequest: %v", err)
	}
	return row
}
