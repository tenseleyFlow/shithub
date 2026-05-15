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

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

const scheduledFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestScheduledIssueCreate_HappyPath verifies a pending row's worker
// run produces a real issue, marks status=created, and stores the
// back-pointer.
func TestScheduledIssueCreate_HappyPath(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	rq := reposdb.New()

	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: scheduledFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "scheduling-target",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	scheduled, err := uq.InsertScheduledIssue(ctx, pool, usersdb.InsertScheduledIssueParams{
		UserID:     user.ID,
		RepoID:     repo.ID,
		Title:      "ship the thing",
		Body:       "details follow",
		ScheduleAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertScheduledIssue: %v", err)
	}

	handler := jobs.ScheduledIssueCreate(jobs.ScheduledIssueCreateDeps{
		Pool:          pool,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:         audit.NewRecorder(),
		IssuesLimiter: throttle.NewLimiter(),
	})
	payload, _ := json.Marshal(map[string]any{"scheduled_id": scheduled.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("handler: %v", err)
	}

	got, err := uq.GetScheduledIssueByID(ctx, pool, scheduled.ID)
	if err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if got.Status != usersdb.ScheduledIssueStatusCreated {
		t.Errorf("status: got %q, want created", got.Status)
	}
	if !got.CreatedIssueID.Valid {
		t.Errorf("created_issue_id not set")
	}
}

// TestScheduledIssueCreate_CancelledShortCircuits verifies a cancelled
// schedule produces a clean no-op (no new issue row, status stays
// 'cancelled', no error returned).
func TestScheduledIssueCreate_CancelledShortCircuits(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	rq := reposdb.New()
	user, _ := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: scheduledFixtureHash,
	})
	repo, _ := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "scheduling-cancel",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	scheduled, _ := uq.InsertScheduledIssue(ctx, pool, usersdb.InsertScheduledIssueParams{
		UserID:     user.ID,
		RepoID:     repo.ID,
		Title:      "do not ship",
		Body:       "user cancelled",
		ScheduleAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Hour), Valid: true},
	})
	if err := uq.CancelScheduledIssue(ctx, pool, usersdb.CancelScheduledIssueParams{
		ID: scheduled.ID, UserID: user.ID,
	}); err != nil {
		t.Fatalf("CancelScheduledIssue: %v", err)
	}

	handler := jobs.ScheduledIssueCreate(jobs.ScheduledIssueCreateDeps{
		Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit: audit.NewRecorder(), IssuesLimiter: throttle.NewLimiter(),
	})
	payload, _ := json.Marshal(map[string]any{"scheduled_id": scheduled.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("handler should no-op on cancelled, got: %v", err)
	}

	got, _ := uq.GetScheduledIssueByID(ctx, pool, scheduled.ID)
	if got.Status != usersdb.ScheduledIssueStatusCancelled {
		t.Errorf("status: got %q, want cancelled", got.Status)
	}
	if got.CreatedIssueID.Valid {
		t.Errorf("cancelled row should not produce an issue")
	}
}

// TestScheduledIssueCreate_AlreadyCreatedIsIdempotent verifies a retry
// after a successful run is a no-op rather than a duplicate-issue.
func TestScheduledIssueCreate_AlreadyCreatedIsIdempotent(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	rq := reposdb.New()
	user, _ := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "carol", DisplayName: "Carol", PasswordHash: scheduledFixtureHash,
	})
	repo, _ := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "scheduling-idempotent",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	scheduled, _ := uq.InsertScheduledIssue(ctx, pool, usersdb.InsertScheduledIssueParams{
		UserID:     user.ID,
		RepoID:     repo.ID,
		Title:      "first run",
		Body:       "first body",
		ScheduleAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(-time.Minute), Valid: true},
	})

	handler := jobs.ScheduledIssueCreate(jobs.ScheduledIssueCreateDeps{
		Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit: audit.NewRecorder(), IssuesLimiter: throttle.NewLimiter(),
	})
	payload, _ := json.Marshal(map[string]any{"scheduled_id": scheduled.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("first run: %v", err)
	}

	row1, _ := uq.GetScheduledIssueByID(ctx, pool, scheduled.ID)
	issueID1 := row1.CreatedIssueID.Int64

	if err := handler(ctx, payload); err != nil {
		t.Fatalf("retry should no-op: %v", err)
	}
	row2, _ := uq.GetScheduledIssueByID(ctx, pool, scheduled.ID)
	if row2.CreatedIssueID.Int64 != issueID1 {
		t.Errorf("retry produced a different issue id: %d vs %d", row2.CreatedIssueID.Int64, issueID1)
	}
}
