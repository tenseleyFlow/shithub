// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
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

func TestRepoTabActionsRendersDispatchWorkflowsForWriters(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.seedWorkflowFile(t, "manual.yml", dispatchWorkflowFixture)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"DISPATCH=Manual:/alice/public-repo/actions/workflows/manual.yml/dispatches:",
		"env/choice/true//staging|prod|,",
		"dry_run/boolean/false/true/,",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("owner body missing %q in %s", want, body)
		}
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions", nil)
	f.actionsMux(viewerFor(f.stranger)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("stranger status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "DISPATCH=") {
		t.Fatalf("dispatch controls leaked to non-writer: %s", resp.Body.String())
	}
}

func TestRepoActionsDispatchAcceptsFormInputs(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.seedWorkflowFile(t, "manual.yml", dispatchWorkflowFixture)

	form := url.Values{}
	form.Set("ref", "trunk")
	form.Set("inputs.env", "prod")
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/actions/workflows/manual.yml/dispatches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := httptest.NewRecorder()
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if loc := resp.Header().Get("Location"); loc != "/alice/public-repo/actions?workflow=.shithub%2Fworkflows%2Fmanual.yml&event=workflow_dispatch" {
		t.Fatalf("Location=%q", loc)
	}

	var raw []byte
	err := f.pool.QueryRow(context.Background(), `
		SELECT event_payload
		FROM workflow_runs
		WHERE repo_id = $1 AND workflow_file = '.shithub/workflows/manual.yml'`,
		f.publicRepo.ID,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("select workflow dispatch run: %v", err)
	}
	var payload map[string]map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if got := payload["inputs"]["env"]; got != "prod" {
		t.Fatalf("env input=%q", got)
	}
	if got := payload["inputs"]["dry_run"]; got != "true" {
		t.Fatalf("dry_run default=%q", got)
	}
}

func TestNormalizeDispatchInputsRejectsUnknownAndInvalidChoice(t *testing.T) {
	t.Parallel()
	specs := dispatchWorkflowInputSpecs()
	if _, err := normalizeDispatchInputs(map[string]string{"bogus": "x"}, specs); err == nil {
		t.Fatal("unknown input accepted")
	}
	if _, err := normalizeDispatchInputs(map[string]string{"env": "qa"}, specs); err == nil {
		t.Fatal("invalid choice accepted")
	}
	if _, err := normalizeDispatchInputs(nil, specs); err == nil {
		t.Fatal("missing required input accepted")
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

func TestRepoActionRunShowsQueuedRunnerLabelWaitReason(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      8,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusQueued,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -2 * time.Minute,
	}, now)
	f.insertWorkflowJob(t, workflowJobFixture{
		RunID:    runID,
		JobIndex: 0,
		JobKey:   "windows",
		JobName:  "Windows",
		RunsOn:   "windows-latest",
		Status:   actionsdb.WorkflowJobStatusQueued,
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/8", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "WAIT=Waiting for runner with labels: windows-latest;") {
		t.Fatalf("wait reason missing: %s", body)
	}
}

func TestRepoActionRunRendersCancelControlsForWritersOnly(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      12,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusRunning,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -5 * time.Minute,
		StartedOffset: -4 * time.Minute,
	}, now)
	f.insertWorkflowJob(t, workflowJobFixture{
		RunID:    runID,
		JobIndex: 0,
		JobKey:   "build",
		JobName:  "Build",
		RunsOn:   "ubuntu-latest",
		Status:   actionsdb.WorkflowJobStatusQueued,
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/12", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"CANCEL_RUN=/alice/public-repo/actions/runs/12/cancel;",
		"CANCEL_JOB=/alice/public-repo/actions/runs/12/jobs/0/cancel;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("owner body missing %q in %s", want, body)
		}
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/12", nil)
	f.actionsMux(viewerFor(f.stranger)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("stranger status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "CANCEL_") {
		t.Fatalf("cancel controls leaked to non-writer: %s", resp.Body.String())
	}
}

func TestRepoActionRunCancelCancelsQueuedRun(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      13,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusQueued,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -5 * time.Minute,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:    runID,
		JobIndex: 0,
		JobKey:   "build",
		JobName:  "Build",
		RunsOn:   "ubuntu-latest",
		Status:   actionsdb.WorkflowJobStatusQueued,
	})
	stepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:      jobID,
		StepIndex:  0,
		RunCommand: "go test ./...",
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/actions/runs/13/cancel", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if loc := resp.Header().Get("Location"); loc != "/alice/public-repo/actions/runs/13" {
		t.Fatalf("Location=%q", loc)
	}
	job, err := actionsdb.New().GetWorkflowJobByID(context.Background(), f.pool, jobID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.Status != actionsdb.WorkflowJobStatusCancelled || !job.CancelRequested {
		t.Fatalf("job: %+v", job)
	}
	step, err := actionsdb.New().GetWorkflowStepByID(context.Background(), f.pool, stepID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID: %v", err)
	}
	if step.Status != actionsdb.WorkflowStepStatusCancelled {
		t.Fatalf("step: %+v", step)
	}
	run, err := actionsdb.New().GetWorkflowRunByID(context.Background(), f.pool, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusCompleted ||
		!run.Conclusion.Valid || run.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("run: %+v", run)
	}
}

func TestRepoActionRunRendersRerunControlsForWritersOnly(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      14,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionFailure,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -20 * time.Minute,
		StartedOffset: -19 * time.Minute,
		DoneOffset:    -18 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/14", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "RERUN=/alice/public-repo/actions/runs/14/rerun;") {
		t.Fatalf("owner body missing rerun control: %s", body)
	}
	if strings.Contains(body, "CANCEL_RUN=") {
		t.Fatalf("terminal run rendered cancel control: %s", body)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/14", nil)
	f.actionsMux(viewerFor(f.stranger)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("stranger status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "RERUN=") {
		t.Fatalf("rerun control leaked to non-writer: %s", resp.Body.String())
	}
}

func TestRepoActionRunRerunQueuesOriginalCommitWorkflow(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	oldSHA := f.seedWorkflowFile(t, "ci.yml", rerunOldWorkflow)
	gitDir, err := f.handlers.d.RepoFS.RepoPath(f.owner.Username, f.publicRepo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	_, err = (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "Change workflow",
		When:        now.Add(5 * time.Minute),
		Files: []repogit.FileEntry{
			{Path: ".shithub/workflows/ci.yml", Body: []byte(rerunNewWorkflow)},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build new workflow: %v", err)
	}
	sourceRunID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      15,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadSHA:       oldSHA,
		HeadRef:       "refs/heads/trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		EventPayload:  `{"ref":"refs/heads/trunk"}`,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionFailure,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -20 * time.Minute,
		StartedOffset: -19 * time.Minute,
		DoneOffset:    -18 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/actions/runs/15/rerun", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if loc := resp.Header().Get("Location"); loc != "/alice/public-repo/actions/runs/16" {
		t.Fatalf("Location=%q", loc)
	}

	rerun, err := actionsdb.New().GetWorkflowRunForRepoByIndex(context.Background(), f.pool, actionsdb.GetWorkflowRunForRepoByIndexParams{
		RepoID:   f.publicRepo.ID,
		RunIndex: 16,
	})
	if err != nil {
		t.Fatalf("GetWorkflowRunForRepoByIndex rerun: %v", err)
	}
	if !rerun.ParentRunID.Valid || rerun.ParentRunID.Int64 != sourceRunID || rerun.HeadSha != oldSHA {
		t.Fatalf("rerun row: %+v source=%d oldSHA=%s", rerun, sourceRunID, oldSHA)
	}
	jobs, err := actionsdb.New().ListJobsForRun(context.Background(), f.pool, rerun.ID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobKey != "old_job" {
		t.Fatalf("rerun jobs came from wrong workflow: %+v", jobs)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/16", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("rerun detail status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "PARENT=15:/alice/public-repo/actions/runs/15;") {
		t.Fatalf("rerun detail missing parent link: %s", resp.Body.String())
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
		"STREAM=/alice/public-repo/actions/runs/9/jobs/0/steps/0/log/stream?after=1;",
		"LOG=hello\nworld\n;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func TestRepoActionStepLogStreamResumesAndClosesForTerminalStep(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      11,
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
		StepName:    "Run",
		RunCommand:  "printf done",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		StartedAt:   now.Add(-3 * time.Minute),
		CompletedAt: now.Add(-1 * time.Minute),
	})
	f.insertStepLogChunk(t, stepID, 0, "hello\n")
	f.insertStepLogChunk(t, stepID, 1, "world\n")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/11/jobs/0/steps/0/log/stream?after=0", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	body := resp.Body.String()
	for _, want := range []string{
		"id: 1\n",
		"event: chunk\n",
		`"chunk_b64":"d29ybGQK"`,
		"event: done\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "aGVsbG8K") {
		t.Fatalf("stream replayed chunk before Last-Event-ID: %s", body)
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
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}/log/stream", f.handlers.repoActionStepLogStream)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}", f.handlers.repoActionStepLog)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/status", f.handlers.repoActionRunStatus)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}", f.handlers.repoActionRun)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/cancel", f.handlers.repoActionRunCancel)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/rerun", f.handlers.repoActionRunRerun)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/cancel", f.handlers.repoActionJobCancel)
	mux.Post("/{owner}/{repo}/actions/workflows/{file}/dispatches", f.handlers.repoActionsDispatch)
	mux.Get("/{owner}/{repo}/actions", f.handlers.repoTabActions)
	return mux
}

const dispatchWorkflowFixture = `name: Manual
on:
  workflow_dispatch:
    inputs:
      env:
        description: Environment
        required: true
        type: choice
        options:
          - staging
          - prod
      dry_run:
        description: Dry run
        type: boolean
        default: "true"
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`

const rerunOldWorkflow = `name: CI
on: push
jobs:
  old_job:
    name: Old job
    runs-on: ubuntu-latest
    steps:
      - run: echo old
`

const rerunNewWorkflow = `name: CI
on: push
jobs:
  new_job:
    name: New job
    runs-on: ubuntu-latest
    steps:
      - run: echo new
`

func dispatchWorkflowInputSpecs() []workflow.DispatchInput {
	return []workflow.DispatchInput{
		{
			Name:     "env",
			Type:     "choice",
			Required: true,
			Options:  []string{"staging", "prod"},
		},
		{
			Name:    "dry_run",
			Type:    "boolean",
			Default: "true",
		},
	}
}

func (f *repoFixture) seedWorkflowFile(t *testing.T, name, body string) string {
	t.Helper()
	ctx := context.Background()
	gitDir, err := f.handlers.d.RepoFS.RepoPath(f.owner.Username, f.publicRepo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := f.handlers.d.RepoFS.InitBare(ctx, gitDir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	commit, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "Add workflow",
		When:        time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		Files: []repogit.FileEntry{
			{Path: ".shithub/workflows/" + name, Body: []byte(body)},
		},
	}).Build(ctx)
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return commit
}

type workflowRunFixture struct {
	RunIndex      int64
	WorkflowFile  string
	WorkflowName  string
	HeadSHA       string
	HeadRef       string
	Event         actionsdb.WorkflowRunEvent
	EventPayload  string
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
	headSHA := fx.HeadSHA
	if headSHA == "" {
		headSHA = strings.Repeat(strconvDigit(fx.RunIndex), 40)
	}
	eventPayload := fx.EventPayload
	if eventPayload == "" {
		eventPayload = "{}"
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
			$5, $6, $7, $8::jsonb, $9,
			$10, $11, $12, $13, $14, $15
		)
		RETURNING id`,
		repoID,
		fx.RunIndex,
		fx.WorkflowFile,
		fx.WorkflowName,
		headSHA,
		fx.HeadRef,
		fx.Event,
		eventPayload,
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
