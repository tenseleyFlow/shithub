// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

const secretScanFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestSecretScanHistory_FindsCuratedPatterns runs the worker against a
// repo whose default branch contains a file with a credential-shaped
// fixture, and asserts the finding lands.
func TestSecretScanHistory_FindsCuratedPatterns(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, false /* free */, true /* upgrade owner */)

	// Seed a file with an AWS-shaped fixture. Stick the literal in
	// runtime concatenation so GitHub push-protection doesn't trip
	// on our own source.
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	if err := env.run(); err != nil {
		t.Fatalf("worker: %v", err)
	}

	count := countFindings(t, env.pool, env.repoID, "")
	if count == 0 {
		t.Fatalf("expected at least 1 finding, got 0")
	}
	pattern := firstFindingPattern(t, env.pool, env.repoID)
	if pattern != "aws-access-key-id" {
		t.Errorf("pattern: got %q, want aws-access-key-id", pattern)
	}
}

// TestSecretScanHistory_OwnerGateFreeReportOnly pins the report-only
// path: a Free-owned repo still gets scanned (the would-deny logs).
func TestSecretScanHistory_OwnerGateFreeReportOnly(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, false /* report-only */, false /* keep free */)
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	if err := env.run(); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if count := countFindings(t, env.pool, env.repoID, ""); count == 0 {
		t.Fatalf("report-only Free-owned repo should still scan; got 0 findings")
	}
}

// TestSecretScanHistory_OwnerGateFreeEnforced pins the enforce path:
// a Free-owned repo is skipped without scanning.
func TestSecretScanHistory_OwnerGateFreeEnforced(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, true /* enforce */, false /* keep free */)
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	if err := env.run(); err != nil {
		t.Fatalf("worker should no-op gracefully: %v", err)
	}
	if count := countFindings(t, env.pool, env.repoID, ""); count != 0 {
		t.Fatalf("enforce mode on Free-owned repo should produce zero findings, got %d", count)
	}
}

// TestSecretScanHistory_IdempotentRescan pins the upsert behaviour:
// running twice on the same repo doesn't create duplicate rows.
func TestSecretScanHistory_IdempotentRescan(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, false, true)
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	if err := env.run(); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	first := countFindings(t, env.pool, env.repoID, "")
	if err := env.run(); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	second := countFindings(t, env.pool, env.repoID, "")
	if second != first {
		t.Errorf("rescan produced duplicates: first=%d, second=%d", first, second)
	}
}

// TestSecretScanHistory_AllowlistSkipsMatch confirms that a
// (pattern, path) row in secret_scan_allowlist prevents the worker
// from writing a corresponding finding.
func TestSecretScanHistory_AllowlistSkipsMatch(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, false, true)
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	// Pre-seed the allowlist so the scan that follows is gated.
	if _, err := env.pool.Exec(
		context.Background(),
		`INSERT INTO secret_scan_allowlist (repo_id, pattern, path, reason)
		 VALUES ($1, 'aws-access-key-id', 'config.json', 'fixture')`,
		env.repoID,
	); err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}

	if err := env.run(); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if count := countFindings(t, env.pool, env.repoID, ""); count != 0 {
		t.Errorf("allowlisted (pattern, path) should yield 0 findings, got %d", count)
	}
}

// TestSecretScanHistory_RedactedExcerptStored asserts the excerpt
// column never carries the raw matched bytes. This is the single most
// important invariant of the finding storage.
func TestSecretScanHistory_RedactedExcerptStored(t *testing.T) {
	t.Parallel()
	env := setupSecretScanEnv(t, false, true)
	seedRepoFile(t, env.gitDir, "config.json", "AWS_KEY="+"AKIAIOSFODNN7EXAMPLE\n", "Initial commit")

	if err := env.run(); err != nil {
		t.Fatalf("worker: %v", err)
	}

	var excerpt string
	if err := env.pool.QueryRow(
		context.Background(),
		`SELECT excerpt FROM secret_scan_findings WHERE repo_id = $1 LIMIT 1`, env.repoID,
	).Scan(&excerpt); err != nil {
		t.Fatalf("read excerpt: %v", err)
	}
	if strings.Contains(excerpt, "AKIA"+"IOSFODNN7EXAMPLE") {
		t.Fatalf("excerpt leaked raw secret bytes: %q", excerpt)
	}
	if !strings.Contains(excerpt, "[REDACTED]") {
		t.Fatalf("excerpt missing redaction marker: %q", excerpt)
	}
}

// secretScanEnv bundles the per-test pool + RepoFS + repo id + a
// closure to invoke the worker. Each test gets its own.
type secretScanEnv struct {
	pool   *pgxpool.Pool
	repoID int64
	gitDir string
	run    func() error
}

// setupSecretScanEnv builds the fixture. enforce controls the
// EnforceConfig flag; upgradeOwner promotes the user to Pro so the
// entitlement gate allows the scan.
func setupSecretScanEnv(t *testing.T, enforce bool, upgradeOwner bool) *secretScanEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	owner, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "scanowner", DisplayName: "Scan Owner", PasswordHash: secretScanFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if upgradeOwner {
		upgradeSecretScanOwnerToPro(t, pool, owner.ID)
	}

	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "scanme",
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

	enforceCfg := config.EnforceConfig{}
	if enforce {
		enforceCfg.UserSecretScanHistory = true
	}

	run := func() error {
		handler := jobs.SecretScanHistory(jobs.SecretScanHistoryDeps{
			Pool:    pool,
			RepoFS:  rfs,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			Enforce: enforceCfg,
		})
		payload, _ := json.Marshal(map[string]any{"repo_id": repo.ID})
		return handler(ctx, payload)
	}

	return &secretScanEnv{pool: pool, repoID: repo.ID, gitDir: gitDir, run: run}
}

func upgradeSecretScanOwnerToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_secscan_pro", Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_secscan_pro",
	})
	if err != nil {
		t.Fatalf("upgrade owner to Pro: %v", err)
	}
}

// initBareRepo runs `git init --bare --initial-branch=trunk` at path.
func initBareRepo(path string) error {
	//nolint:gosec // G204: t.TempDir-derived path + fixed flags.
	cmd := exec.Command("git", "init", "--bare", "--initial-branch=trunk", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init: %w\n%s", err, out)
	}
	return nil
}

// seedRepoFile writes a single-file initial commit to gitDir.
func seedRepoFile(t *testing.T, gitDir, path, body, msg string) {
	t.Helper()
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	ic := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Tester",
		AuthorEmail: "test@example.com",
		Message:     msg,
		Branch:      "trunk",
		When:        when,
		Files: []repogit.FileEntry{
			{Path: path, Body: []byte(body)},
		},
	}
	if _, err := ic.Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
}

func countFindings(t *testing.T, pool *pgxpool.Pool, repoID int64, status string) int64 {
	t.Helper()
	var got int64
	query := `SELECT count(*) FROM secret_scan_findings WHERE repo_id = $1`
	args := []any{repoID}
	if status != "" {
		query += ` AND status::text = $2`
		args = append(args, status)
	}
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return got
}

func firstFindingPattern(t *testing.T, pool *pgxpool.Pool, repoID int64) string {
	t.Helper()
	var pattern string
	if err := pool.QueryRow(
		context.Background(),
		`SELECT pattern FROM secret_scan_findings WHERE repo_id = $1 ORDER BY id LIMIT 1`, repoID,
	).Scan(&pattern); err != nil {
		t.Fatalf("read pattern: %v", err)
	}
	return pattern
}
