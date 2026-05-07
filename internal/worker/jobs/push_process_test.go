// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestPushProcess_HappyPath exercises the full push:process pipeline
// against real Postgres + real bare repo. Verifies that the handler:
//   - sets repos.default_branch_oid when the ref is the default branch,
//   - inserts a webhook_events_pending row,
//   - marks the push_event processed_at,
//   - enqueues a follow-up repo:size_recalc job.
func TestPushProcess_HappyPath(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	// User + verified email so repos.Create accepts a templated initial
	// commit (the create path needs author identity for plumbing).
	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "alice@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	_ = uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	})

	res, err := repos.Create(context.Background(), repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: "alice",
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	// Insert a push_event covering the initial commit on refs/heads/trunk.
	wq := workerdb.New()
	event, err := wq.InsertPushEvent(context.Background(), pool, workerdb.InsertPushEventParams{
		RepoID:       res.Repo.ID,
		BeforeSha:    strings.Repeat("0", 40),
		AfterSha:     res.InitialCommitOID,
		Ref:          "refs/heads/trunk",
		Protocol:     "ssh",
		PusherUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		RequestID:    pgtype.Text{String: "test-req", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPushEvent: %v", err)
	}

	// Run the handler directly.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	handler := jobs.PushProcess(jobs.PushProcessDeps{Pool: pool, RepoFS: rfs, Logger: logger})
	payload, _ := json.Marshal(jobs.PushProcessPayload{PushEventID: event.ID})
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("push:process: %v", err)
	}

	// Default branch OID should now match the initial commit.
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(context.Background(), pool, res.Repo.ID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	if !repo.DefaultBranchOid.Valid || repo.DefaultBranchOid.String != res.InitialCommitOID {
		t.Errorf("default_branch_oid = %v, want %q", repo.DefaultBranchOid, res.InitialCommitOID)
	}

	// Push event marked processed.
	got, err := wq.GetPushEvent(context.Background(), pool, event.ID)
	if err != nil {
		t.Fatalf("GetPushEvent: %v", err)
	}
	if !got.ProcessedAt.Valid {
		t.Errorf("processed_at not set")
	}

	// Webhook event row exists.
	var webhookCount int
	row := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_events_pending WHERE repo_id = $1 AND event_kind = 'push'`,
		res.Repo.ID)
	if err := row.Scan(&webhookCount); err != nil {
		t.Fatalf("count webhook_events_pending: %v", err)
	}
	if webhookCount != 1 {
		t.Errorf("webhook_events_pending count = %d, want 1", webhookCount)
	}

	// repo:size_recalc job enqueued.
	var sizeJobCount int
	row = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'repo:size_recalc' AND completed_at IS NULL AND failed_at IS NULL`)
	_ = row.Scan(&sizeJobCount)
	if sizeJobCount < 1 {
		t.Errorf("repo:size_recalc not enqueued (count=%d)", sizeJobCount)
	}

	// Sanity: re-running is a no-op (idempotent on processed_at).
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// TestRepoSizeRecalc_UpdatesDiskUsedBytes drives the size recalc end-to-
// end against a real repo and verifies disk_used_bytes is non-zero.
func TestRepoSizeRecalc_UpdatesDiskUsedBytes(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	user, _ := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "bob", PasswordHash: fixtureHash,
	})
	em, _ := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "bob@example.com", IsPrimary: true, Verified: true,
	})
	_ = uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	})
	res, err := repos.Create(context.Background(), repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: "bob",
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	handler := jobs.RepoSizeRecalc(jobs.RepoSizeRecalcDeps{Pool: pool, RepoFS: rfs, Logger: logger})
	payload, _ := json.Marshal(jobs.RepoSizeRecalcPayload{RepoID: res.Repo.ID})
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("repo:size_recalc: %v", err)
	}

	rq := reposdb.New()
	repo, _ := rq.GetRepoByID(context.Background(), pool, res.Repo.ID)
	if repo.DiskUsedBytes <= 0 {
		t.Errorf("disk_used_bytes = %d, want > 0", repo.DiskUsedBytes)
	}
}

// TestPushProcess_BranchNotDefault: a push to refs/heads/feat shouldn't
// overwrite default_branch_oid on a repo whose default_branch is trunk.
func TestPushProcess_BranchNotDefault(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, _ := storage.NewRepoFS(root)
	uq := usersdb.New()
	user, _ := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "carol", DisplayName: "carol", PasswordHash: fixtureHash,
	})
	em, _ := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "carol@example.com", IsPrimary: true, Verified: true,
	})
	_ = uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	})
	res, _ := repos.Create(context.Background(), repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: "carol",
		Name: "demo", Visibility: "public", InitReadme: true,
	})

	wq := workerdb.New()
	event, err := wq.InsertPushEvent(context.Background(), pool, workerdb.InsertPushEventParams{
		RepoID:       res.Repo.ID,
		BeforeSha:    strings.Repeat("0", 40),
		AfterSha:     "deadbeef" + strings.Repeat("0", 32),
		Ref:          "refs/heads/feat",
		Protocol:     "ssh",
		PusherUserID: pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPushEvent: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	handler := jobs.PushProcess(jobs.PushProcessDeps{Pool: pool, RepoFS: rfs, Logger: logger})
	payload, _ := json.Marshal(jobs.PushProcessPayload{PushEventID: event.ID})
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("push:process: %v", err)
	}
	rq := reposdb.New()
	repo, _ := rq.GetRepoByID(context.Background(), pool, res.Repo.ID)
	if repo.DefaultBranchOid.Valid {
		t.Errorf("default_branch_oid set to %q for non-default ref", repo.DefaultBranchOid.String)
	}
}

// Sanity check that the package's core helpers don't break under nil
// payload (defensive — production hooks always send populated payloads).
func TestPushProcess_RejectsBadPayload(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	rfs, _ := storage.NewRepoFS(t.TempDir())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	handler := jobs.PushProcess(jobs.PushProcessDeps{Pool: pool, RepoFS: rfs, Logger: logger})

	// Empty payload → poison.
	if err := handler(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("empty payload: want error, got nil")
	}
	// Reference unknown event → poison.
	if err := handler(context.Background(), json.RawMessage(`{"push_event_id": 99999}`)); err == nil {
		t.Errorf("missing event: want error, got nil")
	}
}

// Belt + braces: when InitReadme=true the initial commit has a real OID
// matching the on-disk HEAD, which validates our test fixtures match
// reality.
func TestRepoFixture_HeadMatchesInitialCommit(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, _ := storage.NewRepoFS(root)
	uq := usersdb.New()
	user, _ := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "dave", DisplayName: "dave", PasswordHash: fixtureHash,
	})
	em, _ := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "dave@example.com", IsPrimary: true, Verified: true,
	})
	_ = uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	})
	res, _ := repos.Create(context.Background(), repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: "dave",
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	gitDir, _ := rfs.RepoPath("dave", "demo")
	head, found, err := repogit.HeadOf(context.Background(), gitDir, "trunk")
	if err != nil || !found {
		t.Fatalf("HeadOf trunk: found=%v err=%v", found, err)
	}
	if head.OID != res.InitialCommitOID {
		t.Errorf("HeadOf.OID = %q, want %q", head.OID, res.InitialCommitOID)
	}
	// brevity check so the linter is happy with imports.
	_ = time.Second
}
