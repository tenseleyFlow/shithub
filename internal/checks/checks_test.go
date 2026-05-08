// SPDX-License-Identifier: AGPL-3.0-or-later

package checks_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/checks"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fx struct {
	pool   *pgxpool.Pool
	deps   checks.Deps
	repoID int64
}

func setup(t *testing.T) fx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return fx{
		pool:   pool,
		deps:   checks.Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		repoID: repo.ID,
	}
}

func TestCreate_AutoCreatesSuite(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	run, err := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID:  f.repoID,
		HeadSHA: strings.Repeat("a", 40),
		Name:    "lint",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if run.SuiteID == 0 {
		t.Errorf("SuiteID should be set")
	}
	if run.Status != checksdb.CheckStatusQueued {
		t.Errorf("default status: got %s, want queued", run.Status)
	}
	suites, _ := checksdb.New().ListCheckSuitesForCommit(ctx, f.pool, checksdb.ListCheckSuitesForCommitParams{
		RepoID: f.repoID, HeadSha: run.HeadSha,
	})
	if len(suites) != 1 || suites[0].AppSlug != "external" {
		t.Errorf("expected one external suite, got %v", suites)
	}
}

func TestCreate_IdempotentByExternalID(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	args := checks.CreateParams{
		RepoID:     f.repoID,
		HeadSHA:    strings.Repeat("a", 40),
		Name:       "lint",
		ExternalID: "ci-job-42",
	}
	a, err := checks.Create(ctx, f.deps, args)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	b, err := checks.Create(ctx, f.deps, args)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("expected same id (idempotent), got %d vs %d", a.ID, b.ID)
	}
}

func TestCreate_RequiresConclusionWhenCompleted(t *testing.T) {
	f := setup(t)
	_, err := checks.Create(context.Background(), f.deps, checks.CreateParams{
		RepoID:  f.repoID,
		HeadSHA: strings.Repeat("a", 40),
		Name:    "lint",
		Status:  "completed",
	})
	if err == nil {
		t.Errorf("expected ErrCompletedNeedsConclusion, got nil")
	}
}

func TestCreate_RejectsShortHeadSHA(t *testing.T) {
	f := setup(t)
	_, err := checks.Create(context.Background(), f.deps, checks.CreateParams{
		RepoID:  f.repoID,
		HeadSHA: "abc",
		Name:    "lint",
	})
	if err == nil {
		t.Errorf("expected ErrShortHeadSHA, got nil")
	}
}

func TestCreate_RejectsTooLargeOutput(t *testing.T) {
	f := setup(t)
	big := strings.Repeat("x", checks.MaxOutputTextBytes+1)
	_, err := checks.Create(context.Background(), f.deps, checks.CreateParams{
		RepoID:  f.repoID,
		HeadSHA: strings.Repeat("a", 40),
		Name:    "lint",
		Output:  checks.Output{Text: big},
	})
	if err == nil {
		t.Errorf("expected ErrOutputTextTooLarge, got nil")
	}
}

func TestUpdate_RollsUpSuiteConclusion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	a, _ := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID: f.repoID, HeadSHA: sha, Name: "lint",
	})
	b, _ := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID: f.repoID, HeadSHA: sha, Name: "test",
	})
	// Complete both as success.
	for _, id := range []int64{a.ID, b.ID} {
		if _, err := checks.Update(ctx, f.deps, checks.UpdateParams{
			RunID:         id,
			HasStatus:     true,
			Status:        "completed",
			HasConclusion: true,
			Conclusion:    "success",
		}); err != nil {
			t.Fatalf("Update %d: %v", id, err)
		}
	}
	suite, _ := checksdb.New().GetCheckSuite(ctx, f.pool, a.SuiteID)
	if suite.Status != checksdb.CheckStatusCompleted {
		t.Errorf("suite status: got %s, want completed", suite.Status)
	}
	if !suite.Conclusion.Valid || suite.Conclusion.CheckConclusion != checksdb.CheckConclusionSuccess {
		t.Errorf("suite conclusion: got %+v, want success", suite.Conclusion)
	}
}

func TestEvaluateRequiredChecks_NoRequired(t *testing.T) {
	f := setup(t)
	got, err := checks.EvaluateRequiredChecks(context.Background(), f.pool, checks.GateInputs{
		RepoID: f.repoID, HeadSHA: strings.Repeat("a", 40),
	})
	if err != nil || !got.Satisfied {
		t.Errorf("no required → satisfied; got %+v err=%v", got, err)
	}
}

func TestEvaluateRequiredChecks_BlocksThenSatisfies(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	in := checks.GateInputs{
		RepoID:        f.repoID,
		HeadSHA:       sha,
		RequiredNames: []string{"lint"},
	}

	// No run yet → blocked.
	got, _ := checks.EvaluateRequiredChecks(ctx, f.pool, in)
	if got.Satisfied {
		t.Errorf("no run yet, expected blocked")
	}
	// Queued run → still blocked.
	run, _ := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID: f.repoID, HeadSHA: sha, Name: "lint",
	})
	got, _ = checks.EvaluateRequiredChecks(ctx, f.pool, in)
	if got.Satisfied {
		t.Errorf("queued run, expected blocked")
	}
	// Complete with failure → still blocked.
	_, _ = checks.Update(ctx, f.deps, checks.UpdateParams{
		RunID: run.ID, HasStatus: true, Status: "completed",
		HasConclusion: true, Conclusion: "failure",
	})
	got, _ = checks.EvaluateRequiredChecks(ctx, f.pool, in)
	if got.Satisfied {
		t.Errorf("failure, expected blocked")
	}
	// Switch to success → satisfied.
	_, _ = checks.Update(ctx, f.deps, checks.UpdateParams{
		RunID: run.ID, HasStatus: true, Status: "completed",
		HasConclusion: true, Conclusion: "success",
	})
	got, _ = checks.EvaluateRequiredChecks(ctx, f.pool, in)
	if !got.Satisfied {
		t.Errorf("success, expected satisfied; reason=%q missing=%v", got.Reason, got.Missing)
	}
}

func TestStaleOnPush_MarksSuites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	prev := strings.Repeat("a", 40)
	// Queue a run on prev → suite status=queued.
	if _, err := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID: f.repoID, HeadSHA: prev, Name: "lint",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := checks.MarkStaleForPreviousHead(ctx, f.deps, f.repoID, prev)
	if err != nil {
		t.Fatalf("MarkStaleForPreviousHead: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 suite marked stale, got %d", n)
	}
	suites, _ := checksdb.New().ListCheckSuitesForCommit(ctx, f.pool, checksdb.ListCheckSuitesForCommitParams{
		RepoID: f.repoID, HeadSha: prev,
	})
	if len(suites) != 1 || !suites[0].Conclusion.Valid ||
		suites[0].Conclusion.CheckConclusion != checksdb.CheckConclusionStale {
		t.Errorf("post-stale: %+v", suites)
	}
}

func TestUpdate_TimestampsRoundTrip(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	when := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	run, _ := checks.Create(ctx, f.deps, checks.CreateParams{
		RepoID: f.repoID, HeadSHA: strings.Repeat("a", 40), Name: "lint",
	})
	if _, err := checks.Update(ctx, f.deps, checks.UpdateParams{
		RunID:          run.ID,
		HasStatus:      true, Status: "completed",
		HasConclusion:  true, Conclusion: "success",
		HasStartedAt:   true, StartedAt: when,
		HasCompletedAt: true, CompletedAt: when.Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := checksdb.New().GetCheckRun(ctx, f.pool, run.ID)
	if !got.StartedAt.Valid || !got.CompletedAt.Valid {
		t.Errorf("timestamps not set: %+v", got)
	}
}
