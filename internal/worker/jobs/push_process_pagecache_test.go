// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/cache/pagecache"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// TestPushProcess_PublishesPagecacheInvalidation verifies the F01
// PR-4 contract: a processed push_event whose BeforeSha is non-zero
// causes the worker to NOTIFY (pagecache_invalidate, {repo_id, oid})
// where oid is the pre-push head. The listener side is wired into
// the web boot — this test stands up a Listen loop directly and
// checks the round-trip.
func TestPushProcess_PublishesPagecacheInvalidation(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "eve", DisplayName: "eve", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "eve@example.com", IsPrimary: true, Verified: true,
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
		OwnerUserID: user.ID, OwnerUsername: "eve",
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	// Push event with a NON-zero BeforeSha — that's the path PR-4
	// publishes on. AfterSha is the real initial commit so the
	// rest of the handler (changed-paths, default-branch update)
	// runs cleanly.
	prePushOID := strings.Repeat("a", 40)
	wq := workerdb.New()
	event, err := wq.InsertPushEvent(context.Background(), pool, workerdb.InsertPushEventParams{
		RepoID:       res.Repo.ID,
		BeforeSha:    prePushOID,
		AfterSha:     res.InitialCommitOID,
		Ref:          "refs/heads/trunk",
		Protocol:     "ssh",
		PusherUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		RequestID:    pgtype.Text{String: "test-req-pagecache", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPushEvent: %v", err)
	}

	// Start the listener BEFORE running the handler so the NOTIFY
	// emitted from inside push:process is captured. LISTEN setup
	// is racy with the publish; give the goroutine a beat to issue
	// its LISTEN statement.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu          sync.Mutex
		gotRepoID   int64
		gotBranchID string
		fired       = make(chan struct{}, 1)
	)
	apply := func(rid int64, oid string) {
		mu.Lock()
		gotRepoID = rid
		gotBranchID = oid
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	}
	go pagecache.Listen(ctx, pool, apply, slog.New(slog.NewTextHandler(io.Discard, nil)))
	time.Sleep(150 * time.Millisecond)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := jobs.PushProcess(jobs.PushProcessDeps{Pool: pool, RepoFS: rfs, Logger: logger})
	payload, _ := json.Marshal(jobs.PushProcessPayload{PushEventID: event.ID})
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("push:process: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("listener never received pagecache invalidation — push:process did not publish")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotRepoID != res.Repo.ID {
		t.Errorf("repo_id in invalidation: got %d, want %d", gotRepoID, res.Repo.ID)
	}
	if gotBranchID != prePushOID {
		t.Errorf("branch_oid in invalidation: got %q, want %q (the pre-push SHA)", gotBranchID, prePushOID)
	}
}

// TestPushProcess_NoInvalidationOnInitialCommit confirms the
// boundary: a push whose BeforeSha is the all-zero SHA (branch
// creation, not an update) MUST NOT emit a pagecache
// invalidation — there's nothing cached at the zero SHA to
// invalidate, and a spurious NOTIFY would still be harmless but
// noisier than necessary. Verifies the !isZeroSHA guard.
func TestPushProcess_NoInvalidationOnInitialCommit(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "frank", DisplayName: "frank", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "frank@example.com", IsPrimary: true, Verified: true,
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
		OwnerUserID: user.ID, OwnerUsername: "frank",
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	wq := workerdb.New()
	event, err := wq.InsertPushEvent(context.Background(), pool, workerdb.InsertPushEventParams{
		RepoID:       res.Repo.ID,
		BeforeSha:    strings.Repeat("0", 40), // <-- zero SHA, branch creation
		AfterSha:     res.InitialCommitOID,
		Ref:          "refs/heads/trunk",
		Protocol:     "ssh",
		PusherUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		RequestID:    pgtype.Text{String: "test-req-zerosha", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertPushEvent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{}, 1)
	apply := func(int64, string) {
		select {
		case fired <- struct{}{}:
		default:
		}
	}
	go pagecache.Listen(ctx, pool, apply, slog.New(slog.NewTextHandler(io.Discard, nil)))
	time.Sleep(150 * time.Millisecond)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := jobs.PushProcess(jobs.PushProcessDeps{Pool: pool, RepoFS: rfs, Logger: logger})
	payload, _ := json.Marshal(jobs.PushProcessPayload{PushEventID: event.ID})
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("push:process: %v", err)
	}

	select {
	case <-fired:
		t.Fatal("zero-SHA push must not publish a pagecache invalidation")
	case <-time.After(800 * time.Millisecond):
		// Expected: no notification arrives. Short timeout because
		// healthy NOTIFY round-trip is <100ms on local Postgres.
	}
}
