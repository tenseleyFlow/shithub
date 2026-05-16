// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	crondb "github.com/tenseleyFlow/shithub/internal/cronworkflow/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

// PRO-EXT01-13b: end-to-end sweep test. Uses the api_test fixture
// helpers (seedBranchesEnv + seedRepoWithWorkflow) to stand up a real
// bare repo with a workflow file, inserts a cron dispatch row with
// next_fire_at in the past, runs SweepOnce, and pins the outcomes:
// workflow_runs row created, dispatch row advanced.

// dispatchYAML declares `on.workflow_dispatch` so trigger.Enqueue
// considers it valid for the schedule event. (trigger.Enqueue parses
// the workflow + matches the event kind; the schedule cron-fire
// path uses EventKind=schedule and the workflow declares
// `on.schedule` OR `on.workflow_dispatch` — we use the dispatch one
// since that's what the user paste-and-go flow assumes.)
const cronWorkflowYAML = `name: CI
on:
  workflow_dispatch:
  schedule:
    - cron: "* * * * *"
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

// insertCronDispatchPast inserts a row with next_fire_at = now - 1m
// so SweepOnce sees it as due.
func insertCronDispatchPast(t *testing.T, pool *pgxpool.Pool, userID, repoID int64, file string) int64 {
	t.Helper()
	past := time.Now().Add(-time.Minute)
	row, err := crondb.New().CreateCronDispatch(context.Background(), pool,
		crondb.CreateCronDispatchParams{
			UserID:       userID,
			RepoID:       repoID,
			WorkflowFile: file,
			Ref:          "refs/heads/trunk",
			CronExpr:     "* * * * *",
			NextFireAt:   pgtype.Timestamptz{Time: past, Valid: true},
		})
	if err != nil {
		t.Fatalf("CreateCronDispatch: %v", err)
	}
	return row.ID
}

func countWorkflowRuns(t *testing.T, pool *pgxpool.Pool, repoID int64, event string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM workflow_runs WHERE repo_id = $1 AND event = $2`,
		repoID, event,
	).Scan(&n); err != nil {
		t.Fatalf("count workflow_runs: %v", err)
	}
	return n
}

func repoIDForAliceDemo(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT r.id FROM repos r JOIN users u ON u.id = r.owner_user_id
		 WHERE u.username = 'alice' AND r.name = 'demo'`,
	).Scan(&id); err != nil {
		t.Fatalf("repo id lookup: %v", err)
	}
	return id
}

func newSweepFireDeps(rfs *storage.RepoFS, logBuf *bytes.Buffer, enforce bool, pool *pgxpool.Pool) cronworkflow.FireDeps {
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return cronworkflow.FireDeps{
		Pool:           pool,
		Logger:         logger,
		RepoFS:         rfs,
		BillingEnforce: config.EnforceConfig{UserCronWorkflowDispatch: enforce},
	}
}

func TestCronSweep_ProUserFiresAndAdvances(t *testing.T) {
	pool, _, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/cron.yml": cronWorkflowYAML,
	})
	userID := ownerIDForAlice(t, pool)
	upgradeUserToActivePro(t, pool, userID) // Pro → entitlement allowed
	repoID := repoIDForAliceDemo(t, pool)

	dispatchID := insertCronDispatchPast(t, pool, userID, repoID,
		".shithub/workflows/cron.yml")

	logBuf := &bytes.Buffer{}
	n, err := cronworkflow.SweepOnce(context.Background(),
		newSweepFireDeps(rfs, logBuf, false, pool))
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("processed: got %d, want 1", n)
	}

	if got := countWorkflowRuns(t, pool, repoID, "schedule"); got != 1 {
		t.Errorf("workflow_runs: got %d schedule rows, want 1", got)
	}

	// Row advanced.
	row, err := crondb.New().GetCronDispatchByID(context.Background(), pool, dispatchID)
	if err != nil {
		t.Fatalf("GetCronDispatchByID: %v", err)
	}
	if row.LastFireStatus != crondb.UserCronDispatchLastStatusFired {
		t.Errorf("last_fire_status: got %q, want fired", row.LastFireStatus)
	}
	if !row.NextFireAt.Time.After(time.Now()) {
		t.Errorf("next_fire_at should be advanced to the future: %v", row.NextFireAt.Time)
	}
}

func TestCronSweep_FreeUserEnforceSkipsButAdvances(t *testing.T) {
	pool, _, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/cron.yml": cronWorkflowYAML,
	})
	userID := ownerIDForAlice(t, pool)
	// Not upgraded — Free user
	repoID := repoIDForAliceDemo(t, pool)
	dispatchID := insertCronDispatchPast(t, pool, userID, repoID,
		".shithub/workflows/cron.yml")

	logBuf := &bytes.Buffer{}
	if _, err := cronworkflow.SweepOnce(context.Background(),
		newSweepFireDeps(rfs, logBuf, true, pool)); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := countWorkflowRuns(t, pool, repoID, "schedule"); got != 0 {
		t.Errorf("workflow_runs: got %d, want 0 (Free + enforce should skip)", got)
	}
	row, _ := crondb.New().GetCronDispatchByID(context.Background(), pool, dispatchID)
	if row.LastFireStatus != crondb.UserCronDispatchLastStatusSkippedEntitlement {
		t.Errorf("last_fire_status: got %q, want skipped_entitlement", row.LastFireStatus)
	}
	out := logBuf.String()
	for _, want := range []string{
		`"msg":"entitlements.report_only_deny"`,
		`"feature":"cron_workflow_dispatch"`,
		`"mode":"enforce"`,
		`"surface":"cron-workflow-dispatch"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n%s", want, out)
		}
	}
}

func TestCronSweep_FreeUserReportOnlyFiresAndLogs(t *testing.T) {
	pool, _, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	seedRepoWithWorkflow(t, gitDir, map[string]string{
		".shithub/workflows/cron.yml": cronWorkflowYAML,
	})
	userID := ownerIDForAlice(t, pool)
	repoID := repoIDForAliceDemo(t, pool)
	insertCronDispatchPast(t, pool, userID, repoID, ".shithub/workflows/cron.yml")

	logBuf := &bytes.Buffer{}
	if _, err := cronworkflow.SweepOnce(context.Background(),
		newSweepFireDeps(rfs, logBuf, false, pool)); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	// Free + report-only fires.
	if got := countWorkflowRuns(t, pool, repoID, "schedule"); got != 1 {
		t.Errorf("workflow_runs: got %d, want 1 (Free + report-only should fire)", got)
	}
	if !strings.Contains(logBuf.String(), `"mode":"report_only"`) {
		t.Errorf("expected report_only mode in log; got %s", logBuf.String())
	}
}

func TestCronSweep_MissingWorkflowMarksSkipped(t *testing.T) {
	pool, _, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	gitDir, _ := rfs.RepoPath("alice", "demo")
	// Don't write a workflow file — the dispatch points at a path
	// that doesn't exist at the ref.
	seedRepoWithWorkflow(t, gitDir, nil)
	userID := ownerIDForAlice(t, pool)
	upgradeUserToActivePro(t, pool, userID)
	repoID := repoIDForAliceDemo(t, pool)
	dispatchID := insertCronDispatchPast(t, pool, userID, repoID,
		".shithub/workflows/missing.yml")

	logBuf := &bytes.Buffer{}
	_, _ = cronworkflow.SweepOnce(context.Background(),
		newSweepFireDeps(rfs, logBuf, false, pool))

	row, _ := crondb.New().GetCronDispatchByID(context.Background(), pool, dispatchID)
	if row.LastFireStatus != crondb.UserCronDispatchLastStatusSkippedMissingWorkflow {
		t.Errorf("last_fire_status: got %q, want skipped_missing_workflow", row.LastFireStatus)
	}
	// Should still advance — don't spin on the same row.
	if !row.NextFireAt.Time.After(time.Now()) {
		t.Errorf("next_fire_at should still advance on missing workflow")
	}
}

func TestCronSweep_DisabledRowSkipped(t *testing.T) {
	pool, _, rfs, _, _, _ := seedBranchesEnv(t, "alice")
	userID := ownerIDForAlice(t, pool)
	repoID := repoIDForAliceDemo(t, pool)
	dispatchID := insertCronDispatchPast(t, pool, userID, repoID,
		".shithub/workflows/cron.yml")
	if err := (cronworkflow.Deps{Pool: pool}).Disable(context.Background(), dispatchID); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	logBuf := &bytes.Buffer{}
	n, err := cronworkflow.SweepOnce(context.Background(),
		newSweepFireDeps(rfs, logBuf, false, pool))
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("disabled row should not be claimed; processed=%d", n)
	}
}

// Silence unused-import warning if io drops from any test.
var _ = io.Discard
