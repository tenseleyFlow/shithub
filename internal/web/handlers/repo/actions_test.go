// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestRepoTabActionsFiltersWorkflowRunsAndSidebar(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      1,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "main",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionSuccess,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -3 * time.Hour,
		StartedOffset: -3 * time.Hour,
		DoneOffset:    -2 * time.Hour,
	}, now)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      2,
		WorkflowFile:  ".shithub/workflows/deploy.yml",
		WorkflowName:  "Deploy",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventWorkflowDispatch,
		Status:        actionsdb.WorkflowRunStatusRunning,
		ActorUserID:   f.stranger.ID,
		CreatedOffset: -90 * time.Minute,
		StartedOffset: -80 * time.Minute,
	}, now)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      3,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "feature",
		Event:         actionsdb.WorkflowRunEventPullRequest,
		Status:        actionsdb.WorkflowRunStatusQueued,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -30 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions?workflow=.shithub/workflows/ci.yml&branch=main&event=push&status=completed&conclusion=success&actor=alice", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"COUNT=3;",
		"FILTERED=1;",
		"PAGE=1-1 of 1;",
		"WF=CI:2:true;",
		"WF=Deploy:1:false;",
		"RUN=CI:#1:push:main:alice:success;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "RUN=Deploy") || strings.Contains(body, "#3:") {
		t.Fatalf("unfiltered run leaked into filtered response: %s", body)
	}
}

func TestRepoTabActionsPaginatesTwentyRuns(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 21; i++ {
		f.insertWorkflowRun(t, workflowRunFixture{
			RunIndex:      int64(i),
			WorkflowFile:  ".shithub/workflows/ci.yml",
			WorkflowName:  "CI",
			HeadRef:       "main",
			Event:         actionsdb.WorkflowRunEventPush,
			Status:        actionsdb.WorkflowRunStatusQueued,
			ActorUserID:   f.owner.ID,
			CreatedOffset: time.Duration(i) * time.Minute,
		}, now)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1 status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if got := strings.Count(body, "RUN="); got != 20 {
		t.Fatalf("page 1 run count=%d body=%s", got, body)
	}
	if !strings.Contains(body, "PAGE=1-20 of 21;") {
		t.Fatalf("page 1 pagination missing: %s", body)
	}
	if strings.Contains(body, "#1:") {
		t.Fatalf("oldest run appeared on page 1: %s", body)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions?page=2", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2 status=%d body=%s", resp.Code, resp.Body.String())
	}
	body = resp.Body.String()
	if got := strings.Count(body, "RUN="); got != 1 {
		t.Fatalf("page 2 run count=%d body=%s", got, body)
	}
	if !strings.Contains(body, "PAGE=21-21 of 21;") || !strings.Contains(body, "#1:") {
		t.Fatalf("page 2 pagination/run missing: %s", body)
	}
}

func TestRepoActionRunRendersWorkflowRunJobsAndSteps(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      7,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionFailure,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -20 * time.Minute,
		StartedOffset: -19 * time.Minute,
		DoneOffset:    -10 * time.Minute,
	}, now)
	buildID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:       runID,
		JobIndex:    0,
		JobKey:      "build",
		JobName:     "Build",
		RunsOn:      "ubuntu-latest",
		Status:      actionsdb.WorkflowJobStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		StartedAt:   now.Add(-19 * time.Minute),
		CompletedAt: now.Add(-15 * time.Minute),
	})
	testID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:       runID,
		JobIndex:    1,
		JobKey:      "test",
		JobName:     "Test",
		RunsOn:      "ubuntu-latest",
		Needs:       []string{"build"},
		Status:      actionsdb.WorkflowJobStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		StartedAt:   now.Add(-14 * time.Minute),
		CompletedAt: now.Add(-10 * time.Minute),
	})
	f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       buildID,
		StepIndex:   0,
		StepName:    "Checkout",
		UsesAlias:   "actions/checkout@v4",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		CompletedAt: now.Add(-18 * time.Minute),
	})
	f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       testID,
		StepIndex:   0,
		RunCommand:  "go test ./...",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		CompletedAt: now.Add(-10 * time.Minute),
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/7", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"RUN=CI:#7:push:alice:failure;",
		"SUMMARY=2:2:1:0;",
		"JOB=Build:success::ubuntu-latest;",
		"STEP=Checkout:success:/alice/public-repo/actions/runs/7/jobs/0/steps/0;",
		"JOB=Test:failure:build:ubuntu-latest;",
		"STEP=go test ./...:failure:/alice/public-repo/actions/runs/7/jobs/1/steps/0;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func TestRepoActionRunStatusRendersPollingFragment(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      8,
		WorkflowFile:  ".shithub/workflows/deploy.yml",
		WorkflowName:  "Deploy",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventWorkflowDispatch,
		Status:        actionsdb.WorkflowRunStatusRunning,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -5 * time.Minute,
		StartedOffset: -4 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/8/status", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	want := "STATUS=running:false:/alice/public-repo/actions/runs/8/status;"
	if body := resp.Body.String(); !strings.Contains(body, want) {
		t.Fatalf("status fragment missing %q in %s", want, body)
	}
}

func TestRepoActionStepLogRendersSQLChunks(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      9,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusRunning,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -5 * time.Minute,
		StartedOffset: -4 * time.Minute,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:     runID,
		JobIndex:  0,
		JobKey:    "build",
		JobName:   "Build",
		RunsOn:    "ubuntu-latest",
		Status:    actionsdb.WorkflowJobStatusRunning,
		StartedAt: now.Add(-4 * time.Minute),
	})
	stepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:      jobID,
		StepIndex:  0,
		StepName:   "Run tests",
		RunCommand: "go test ./...",
		Status:     actionsdb.WorkflowStepStatusRunning,
		StartedAt:  now.Add(-3 * time.Minute),
	})
	f.insertStepLogChunk(t, stepID, 0, "hello\n")
	f.insertStepLogChunk(t, stepID, 1, "world\n")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/9/jobs/0/steps/0", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"STEPLOG=Build:Run tests:SQL chunks::false;",
		"LOG=hello\nworld\n;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func TestRepoActionStepLogRendersArchivedObject(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      10,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionSuccess,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -5 * time.Minute,
		StartedOffset: -4 * time.Minute,
		DoneOffset:    -1 * time.Minute,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:       runID,
		JobIndex:    0,
		JobKey:      "build",
		JobName:     "Build",
		RunsOn:      "ubuntu-latest",
		Status:      actionsdb.WorkflowJobStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		StartedAt:   now.Add(-4 * time.Minute),
		CompletedAt: now.Add(-1 * time.Minute),
	})
	stepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       jobID,
		StepIndex:   0,
		StepName:    "Archive",
		RunCommand:  "printf archived",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		StartedAt:   now.Add(-3 * time.Minute),
		CompletedAt: now.Add(-1 * time.Minute),
	})
	key := "actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs/" + strconv.FormatInt(jobID, 10) + "/steps/" + strconv.FormatInt(stepID, 10) + ".log"
	if _, err := f.objectStore.Put(context.Background(), key, bytes.NewReader([]byte("archived\n")), storage.PutOpts{ContentType: "text/plain; charset=utf-8"}); err != nil {
		t.Fatalf("put log object: %v", err)
	}
	if _, err := actionsdb.New().UpdateWorkflowStepLogObject(context.Background(), f.pool, actionsdb.UpdateWorkflowStepLogObjectParams{
		LogObjectKey: pgtype.Text{String: key, Valid: true},
		LogByteCount: int64(len("archived\n")),
		ID:           stepID,
	}); err != nil {
		t.Fatalf("update log object: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/10/jobs/0/steps/0", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"STEPLOG=Build:Archive:object storage:mem://actions/runs/",
		"LOG=archived\n;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func (f *repoFixture) actionsMux(viewer middleware.CurrentUser) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}", f.handlers.repoActionStepLog)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/status", f.handlers.repoActionRunStatus)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}", f.handlers.repoActionRun)
	mux.Get("/{owner}/{repo}/actions", f.handlers.repoTabActions)
	return mux
}

type workflowRunFixture struct {
	RunIndex      int64
	WorkflowFile  string
	WorkflowName  string
	HeadRef       string
	Event         actionsdb.WorkflowRunEvent
	Status        actionsdb.WorkflowRunStatus
	Conclusion    actionsdb.CheckConclusion
	ActorUserID   int64
	CreatedOffset time.Duration
	StartedOffset time.Duration
	DoneOffset    time.Duration
	RepoID        int64
}

func (f *repoFixture) insertWorkflowRun(t *testing.T, fx workflowRunFixture, base time.Time) int64 {
	t.Helper()
	repoID := fx.RepoID
	if repoID == 0 {
		repoID = f.publicRepo.ID
	}
	createdAt := base.Add(fx.CreatedOffset)
	startedAt := pgtype.Timestamptz{}
	completedAt := pgtype.Timestamptz{}
	conclusion := actionsdb.NullCheckConclusion{}
	if fx.StartedOffset != 0 || fx.Status == actionsdb.WorkflowRunStatusRunning || fx.Status == actionsdb.WorkflowRunStatusCompleted || fx.Status == actionsdb.WorkflowRunStatusCancelled {
		startedAt = pgtype.Timestamptz{Time: base.Add(fx.StartedOffset), Valid: true}
	}
	if fx.DoneOffset != 0 || fx.Status == actionsdb.WorkflowRunStatusCompleted || fx.Status == actionsdb.WorkflowRunStatusCancelled {
		completedAt = pgtype.Timestamptz{Time: base.Add(fx.DoneOffset), Valid: true}
	}
	if fx.Conclusion != "" {
		conclusion = actionsdb.NullCheckConclusion{CheckConclusion: fx.Conclusion, Valid: true}
	}
	var id int64
	err := f.pool.QueryRow(
		context.Background(), `
		INSERT INTO workflow_runs (
			repo_id, run_index, workflow_file, workflow_name,
			head_sha, head_ref, event, event_payload, actor_user_id,
			status, conclusion, started_at, completed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, '{}'::jsonb, $8,
			$9, $10, $11, $12, $13, $14
		)
		RETURNING id`,
		repoID,
		fx.RunIndex,
		fx.WorkflowFile,
		fx.WorkflowName,
		strings.Repeat(strconvDigit(fx.RunIndex), 40),
		fx.HeadRef,
		fx.Event,
		fx.ActorUserID,
		fx.Status,
		conclusion,
		startedAt,
		completedAt,
		createdAt,
		createdAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workflow run %d: %v", fx.RunIndex, err)
	}
	return id
}

func strconvDigit(n int64) string {
	return strconv.FormatInt(n%10, 10)
}

type workflowJobFixture struct {
	RunID       int64
	JobIndex    int32
	JobKey      string
	JobName     string
	RunsOn      string
	Needs       []string
	Status      actionsdb.WorkflowJobStatus
	Conclusion  actionsdb.CheckConclusion
	StartedAt   time.Time
	CompletedAt time.Time
}

func (f *repoFixture) insertWorkflowJob(t *testing.T, fx workflowJobFixture) int64 {
	t.Helper()
	status := fx.Status
	if status == "" {
		status = actionsdb.WorkflowJobStatusQueued
	}
	conclusion := actionsdb.NullCheckConclusion{}
	if fx.Conclusion != "" {
		conclusion = actionsdb.NullCheckConclusion{CheckConclusion: fx.Conclusion, Valid: true}
	}
	startedAt := pgtype.Timestamptz{}
	if !fx.StartedAt.IsZero() {
		startedAt = pgtype.Timestamptz{Time: fx.StartedAt, Valid: true}
	}
	completedAt := pgtype.Timestamptz{}
	if !fx.CompletedAt.IsZero() {
		completedAt = pgtype.Timestamptz{Time: fx.CompletedAt, Valid: true}
	}
	needs := fx.Needs
	if needs == nil {
		needs = []string{}
	}
	runnerID := pgtype.Int8{}
	if status == actionsdb.WorkflowJobStatusRunning || status == actionsdb.WorkflowJobStatusCompleted {
		runnerID = pgtype.Int8{Int64: f.insertWorkflowRunner(t), Valid: true}
	}
	var id int64
	err := f.pool.QueryRow(
		context.Background(), `
		INSERT INTO workflow_jobs (
			run_id, job_index, job_key, job_name, runs_on, needs_jobs,
			runner_id, status, conclusion, started_at, completed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11
		)
		RETURNING id`,
		fx.RunID,
		fx.JobIndex,
		fx.JobKey,
		fx.JobName,
		fx.RunsOn,
		needs,
		runnerID,
		status,
		conclusion,
		startedAt,
		completedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workflow job %s: %v", fx.JobKey, err)
	}
	return id
}

func (f *repoFixture) insertWorkflowRunner(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := f.pool.QueryRow(
		context.Background(), `
		INSERT INTO workflow_runners (name, labels, status)
		VALUES ($1, ARRAY['ubuntu-latest']::text[], 'busy')
		RETURNING id`,
		"runner-"+strconv.FormatInt(time.Now().UnixNano(), 10),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workflow runner: %v", err)
	}
	return id
}

type workflowStepFixture struct {
	JobID       int64
	StepIndex   int32
	StepName    string
	RunCommand  string
	UsesAlias   string
	Status      actionsdb.WorkflowStepStatus
	Conclusion  actionsdb.CheckConclusion
	StartedAt   time.Time
	CompletedAt time.Time
}

func (f *repoFixture) insertWorkflowStep(t *testing.T, fx workflowStepFixture) int64 {
	t.Helper()
	status := fx.Status
	if status == "" {
		status = actionsdb.WorkflowStepStatusQueued
	}
	runCommand := fx.RunCommand
	if runCommand == "" && fx.UsesAlias == "" {
		runCommand = "true"
	}
	conclusion := actionsdb.NullCheckConclusion{}
	if fx.Conclusion != "" {
		conclusion = actionsdb.NullCheckConclusion{CheckConclusion: fx.Conclusion, Valid: true}
	}
	startedAt := pgtype.Timestamptz{}
	if !fx.StartedAt.IsZero() {
		startedAt = pgtype.Timestamptz{Time: fx.StartedAt, Valid: true}
	}
	completedAt := pgtype.Timestamptz{}
	if !fx.CompletedAt.IsZero() {
		completedAt = pgtype.Timestamptz{Time: fx.CompletedAt, Valid: true}
	}
	var id int64
	err := f.pool.QueryRow(
		context.Background(), `
		INSERT INTO workflow_steps (
			job_id, step_index, step_name, run_command, uses_alias,
			status, conclusion, started_at, completed_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9
		)
		RETURNING id`,
		fx.JobID,
		fx.StepIndex,
		fx.StepName,
		runCommand,
		fx.UsesAlias,
		status,
		conclusion,
		startedAt,
		completedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workflow step %d: %v", fx.StepIndex, err)
	}
	return id
}

func (f *repoFixture) insertStepLogChunk(t *testing.T, stepID int64, seq int32, chunk string) {
	t.Helper()
	if _, err := actionsdb.New().AppendStepLogChunk(context.Background(), f.pool, actionsdb.AppendStepLogChunkParams{
		StepID: stepID,
		Seq:    seq,
		Chunk:  []byte(chunk),
	}); err != nil {
		t.Fatalf("insert step log chunk %d: %v", seq, err)
	}
}
