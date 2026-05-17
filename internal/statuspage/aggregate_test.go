// SPDX-License-Identifier: AGPL-3.0-or-later

package statuspage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/statuspage"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// fixtureHash is a static PHC test fixture (zero salt, zero key) —
// not a real credential. Same shape every test file in the repo uses.
const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestAggregate_EmptyPinSet returns Unknown without touching the
// runs table — the Pro user with no pinned repos still gets a
// renderable Summary.
func TestAggregate_EmptyPinSet(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	userID := mkUser(t, pool, "empty-pins")
	got, err := statuspage.Aggregate(ctx, pool, userID, "empty-pins")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got.OverallState != statuspage.StateUnknown {
		t.Errorf("OverallState = %s, want unknown", got.OverallState)
	}
	if len(got.Repos) != 0 {
		t.Errorf("Repos len = %d, want 0", len(got.Repos))
	}
}

// TestAggregate_AllSuccessOK is the happy path: a single pinned repo
// with one successful run on its default branch produces OverallState=ok.
func TestAggregate_AllSuccessOK(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	userID := mkUser(t, pool, "alice")
	repoID := mkUserRepo(t, pool, userID, "site")
	pinRepo(t, pool, userID, repoID, 1)
	mkRun(t, pool, repoID, "refs/heads/trunk", 1, actionsdb.WorkflowRunStatusCompleted,
		actionsdb.CheckConclusionSuccess, time.Now().Add(-1*time.Hour))

	got, err := statuspage.Aggregate(ctx, pool, userID, "alice")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got.OverallState != statuspage.StateOK {
		t.Errorf("OverallState = %s, want ok", got.OverallState)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1", len(got.Repos))
	}
	r := got.Repos[0]
	if r.LatestRun.RunIndex != 1 {
		t.Errorf("LatestRun.RunIndex = %d, want 1", r.LatestRun.RunIndex)
	}
	if r.SuccessRate != 1.0 {
		t.Errorf("SuccessRate = %f, want 1.0", r.SuccessRate)
	}
}

// TestAggregate_MixedDegraded asserts the degraded fallback when
// one pinned repo passes and another fails. This is the most common
// real-world state.
func TestAggregate_MixedDegraded(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	userID := mkUser(t, pool, "bob")
	ok := mkUserRepo(t, pool, userID, "passing")
	fail := mkUserRepo(t, pool, userID, "broken")
	pinRepo(t, pool, userID, ok, 1)
	pinRepo(t, pool, userID, fail, 2)

	now := time.Now().UTC()
	mkRun(t, pool, ok, "refs/heads/trunk", 5, actionsdb.WorkflowRunStatusCompleted,
		actionsdb.CheckConclusionSuccess, now.Add(-1*time.Hour))
	mkRun(t, pool, fail, "refs/heads/trunk", 5, actionsdb.WorkflowRunStatusCompleted,
		actionsdb.CheckConclusionFailure, now.Add(-1*time.Hour))

	got, err := statuspage.Aggregate(ctx, pool, userID, "bob")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got.OverallState != statuspage.StateDegraded {
		t.Errorf("OverallState = %s, want degraded", got.OverallState)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("Repos len = %d, want 2", len(got.Repos))
	}
}

// TestAggregate_IgnoresNonDefaultBranch confirms the aggregator only
// considers runs on the default branch — feature-branch CI noise
// shouldn't pollute the status badge.
func TestAggregate_IgnoresNonDefaultBranch(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	userID := mkUser(t, pool, "carol")
	repoID := mkUserRepo(t, pool, userID, "proj")
	pinRepo(t, pool, userID, repoID, 1)
	// Failure on a feature branch — must NOT count.
	mkRun(t, pool, repoID, "refs/heads/dev", 99, actionsdb.WorkflowRunStatusCompleted,
		actionsdb.CheckConclusionFailure, time.Now().Add(-30*time.Minute))

	got, err := statuspage.Aggregate(ctx, pool, userID, "carol")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	// No runs on trunk → conclusion stays empty → repo doesn't
	// contribute → overall is unknown.
	if got.OverallState != statuspage.StateUnknown {
		t.Errorf("OverallState = %s, want unknown (feature-branch run must be ignored)", got.OverallState)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("Repos len = %d, want 1", len(got.Repos))
	}
	if got.Repos[0].LatestRun.Conclusion != "" {
		t.Errorf("LatestRun.Conclusion = %q, want empty (only feature-branch run exists)", got.Repos[0].LatestRun.Conclusion)
	}
}

// --- helpers ----------------------------------------------------------------

func mkUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	q := usersdb.New()
	u, err := q.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  username,
		PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func mkUserRepo(t *testing.T, pool *pgxpool.Pool, ownerID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		 VALUES ($1, $2, 'public', 'trunk') RETURNING id`,
		ownerID, name).Scan(&id); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return id
}

// pinRepo writes a pin row at the given position. Creates the pin
// set on first call.
func pinRepo(t *testing.T, pool *pgxpool.Pool, userID, repoID int64, position int32) {
	t.Helper()
	q := reposdb.New()
	ctx := context.Background()
	setID, err := q.UpsertProfilePinSetForUser(ctx, pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		t.Fatalf("UpsertProfilePinSetForUser: %v", err)
	}
	if err := q.InsertProfilePin(ctx, pool, reposdb.InsertProfilePinParams{
		SetID:    setID,
		RepoID:   repoID,
		Position: position,
	}); err != nil {
		t.Fatalf("InsertProfilePin: %v", err)
	}
}

// mkRun inserts a workflow_run + updates it to completed with the
// given conclusion + completed_at. InsertWorkflowRun stamps queued
// status; we manually update for tests so we control the conclusion.
func mkRun(
	t *testing.T,
	pool *pgxpool.Pool,
	repoID int64,
	headRef string,
	runIndex int64,
	status actionsdb.WorkflowRunStatus,
	conclusion actionsdb.CheckConclusion,
	completedAt time.Time,
) {
	t.Helper()
	q := actionsdb.New()
	ctx := context.Background()
	run, err := q.InsertWorkflowRun(ctx, pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repoID,
		RunIndex:     runIndex,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "ci",
		HeadSha:      "0000000000000000000000000000000000000000",
		HeadRef:      headRef,
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	startedAt := completedAt.Add(-1 * time.Minute)
	if _, err := pool.Exec(
		ctx,
		`UPDATE workflow_runs SET status = $1, conclusion = $2, started_at = $3, completed_at = $4 WHERE id = $5`,
		status, conclusion,
		pgtype.Timestamptz{Time: startedAt, Valid: true},
		pgtype.Timestamptz{Time: completedAt, Valid: true},
		run.ID,
	); err != nil {
		t.Fatalf("update run: %v", err)
	}
}
