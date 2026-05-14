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

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
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
		"LISTBASE=/alice/public-repo/actions/workflows/ci.yml;",
		"FILTERQ=branch:main event:push status:completed conclusion:success actor:alice;",
		"MENU=event:Event:",
		"WF=CI:2:true:/alice/public-repo/actions/workflows/ci.yml",
		"WF=Deploy:1:false:/alice/public-repo/actions/workflows/deploy.yml",
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

func TestRepoTabActionsQueryTokensResolveWorkflowNames(t *testing.T) {
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
		CreatedOffset: -time.Hour,
		DoneOffset:    -time.Minute,
	}, now)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      2,
		WorkflowFile:  ".shithub/workflows/deploy.yml",
		WorkflowName:  "Deploy",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventWorkflowDispatch,
		Status:        actionsdb.WorkflowRunStatusQueued,
		ActorUserID:   f.stranger.ID,
		CreatedOffset: -30 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions?query=workflow:%22CI%22+branch:main+event:push+is:success+actor:alice", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"FILTERED=1;",
		"LISTBASE=/alice/public-repo/actions/workflows/ci.yml;",
		"FILTERQ=branch:main event:push conclusion:success actor:alice;",
		"WF=CI:1:true:/alice/public-repo/actions/workflows/ci.yml",
		"RUN=CI:#1:push:main:alice:success;",
		"RUNACTIONS=true:false:/alice/public-repo/actions/workflows/ci.yml:/alice/public-repo/blob/main/.shithub/workflows/ci.yml;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "RUN=Deploy") {
		t.Fatalf("workflow token did not filter rows: %s", body)
	}
}

func TestRepoActionsWorkflowRouteSupportsNestedWorkflowPaths(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      4,
		WorkflowFile:  ".shithub/workflows/nightly/ci.yml",
		WorkflowName:  "Nightly CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusRunning,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -15 * time.Minute,
		StartedOffset: -10 * time.Minute,
	}, now)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/workflows/nightly/ci.yml?query=branch:trunk", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"FILTERED=1;",
		"LISTBASE=/alice/public-repo/actions/workflows/nightly/ci.yml;",
		"FILTERQ=branch:trunk;",
		"MENU=event:Event:",
		"WF=Nightly CI:1:true:/alice/public-repo/actions/workflows/nightly/ci.yml",
		"RUN=Nightly CI:#4:push:trunk:alice:running;",
		"RUNACTIONS=false:true:/alice/public-repo/actions/workflows/nightly/ci.yml:/alice/public-repo/blob/trunk/.shithub/workflows/nightly/ci.yml;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func TestActionsWorkflowRouteRejectsTraversal(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"ci.yml":                         true,
		".shithub/workflows/ci.yaml":     true,
		"nightly/ci.yml":                 true,
		"/ci.yml":                        false,
		"../ci.yml":                      false,
		"nightly/../ci.yml":              false,
		".shithub/workflows/../ci.yml":   false,
		".shithub/workflows/nightly.txt": false,
		`nightly\ci.yml`:                 false,
	}
	for raw, wantOK := range tests {
		got, ok := normalizeActionsWorkflowFile(raw)
		if ok != wantOK {
			t.Fatalf("normalizeActionsWorkflowFile(%q) ok=%v want %v got %q", raw, ok, wantOK, got)
		}
		if ok && !strings.HasPrefix(got, dispatch.WorkflowFilesDir) {
			t.Fatalf("normalized path escaped workflow dir: %q", got)
		}
	}
}

func TestParseActionsFilterQuerySupportsQuotedValuesAndAliases(t *testing.T) {
	t.Parallel()
	got := parseActionsFilterQuery(`workflow:"CI Smoke" branch:feature/x is:success actor:"octo cat"`)
	if got.Workflow != "CI Smoke" || got.Branch != "feature/x" || got.Conclusion != "success" || got.Actor != "octo cat" {
		t.Fatalf("parsed query = %+v", got)
	}
	got = parseActionsFilterQuery(`is:queued event:workflow_dispatch status:running`)
	if got.Status != "running" || got.Event != "workflow_dispatch" {
		t.Fatalf("parsed status query = %+v", got)
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

func TestRepoActionsManagementPagesRenderPlaceholdersAndActiveNav(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      1,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  "CI",
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionSuccess,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -time.Hour,
		DoneOffset:    -time.Minute,
	}, now)

	tests := []struct {
		path  string
		key   string
		title string
		empty string
	}{
		{"/alice/public-repo/actions/caches", "caches", "Caches", "No caches"},
		{"/alice/public-repo/actions/attestations", "attestations", "Attestations", "No attestations"},
		{"/alice/public-repo/actions/runners", "runners", "Runners", "Repository runner management is coming later"},
		{"/alice/public-repo/actions/metrics/usage", "usage", "Actions Usage Metrics", "No usage metrics available yet"},
		{"/alice/public-repo/actions/metrics/performance", "performance", "Actions Performance Metrics", "No performance metrics available yet"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			f.actionsMux(viewerFor(f.stranger)).ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			body := resp.Body.String()
			for _, want := range []string{
				"MGMT=" + tt.key + ":" + tt.title + ":" + tt.empty + ";",
				"MGMTNAV=" + tt.key + ":true:",
				"COUNT=1;",
				"WF=CI:false;",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("body missing %q in %s", want, body)
				}
			}
		})
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
	if loc := resp.Header().Get("Location"); loc != "/alice/public-repo/actions/workflows/manual.yml?query=event%3Aworkflow_dispatch" {
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
	if _, err := dispatch.NormalizeInputs(map[string]string{"bogus": "x"}, specs); err == nil {
		t.Fatal("unknown input accepted")
	}
	if _, err := dispatch.NormalizeInputs(map[string]string{"env": "qa"}, specs); err == nil {
		t.Fatal("invalid choice accepted")
	}
	if _, err := dispatch.NormalizeInputs(nil, specs); err == nil {
		t.Fatal("missing required input accepted")
	}
}

func TestActionsRunGraphBuildsEdgesAndStepSummaries(t *testing.T) {
	t.Parallel()
	graph := actionsRunGraph([]actionsJobDetailView{
		{
			JobIndex:   0,
			JobKey:     "build",
			Name:       "Build",
			RunsOn:     "ubuntu-latest",
			StateText:  "Success",
			StateClass: "success",
			StateIcon:  "check-circle",
			Duration:   "2m",
			Anchor:     "job-0",
			Steps: []actionsStepDetailView{
				{Name: "Checkout", Kind: "uses", Detail: "actions/checkout@v4", StateText: "Success", StateClass: "success", Duration: "1s", IsTerminal: true, LogHref: "/steps/0"},
				{Name: "Build", Kind: "run", Detail: "go build ./...", StateText: "Success", StateClass: "success", Duration: "2m", IsTerminal: true, LogHref: "/steps/1"},
			},
		},
		{
			JobIndex:   1,
			JobKey:     "test",
			Name:       "Test",
			RunsOn:     "ubuntu-latest",
			Needs:      []string{"build"},
			NeedsText:  "build",
			StateText:  "Failure",
			StateClass: "failure",
			StateIcon:  "x-circle",
			Duration:   "1m",
			Anchor:     "job-1",
			Steps: []actionsStepDetailView{
				{Name: "Test", Kind: "run", Detail: "go test ./...", StateText: "Failure", StateClass: "failure", Duration: "1m", IsTerminal: true, LogHref: "/steps/2"},
			},
		},
	})
	if len(graph.Nodes) != 2 {
		t.Fatalf("nodes=%d, want 2", len(graph.Nodes))
	}
	if len(graph.Edges) != 1 {
		t.Fatalf("edges=%d, want 1", len(graph.Edges))
	}
	if graph.Edges[0].From != "job-0" || graph.Edges[0].To != "job-1" || graph.Edges[0].Path == "" {
		t.Fatalf("unexpected edge: %+v", graph.Edges[0])
	}
	if got := graph.Nodes[1]; got.StepCount != 1 || got.CompletedStepCount != 1 || got.FailureCount != 1 || got.Steps[0].LogHref != "/steps/2" {
		t.Fatalf("test node summary = %+v", got)
	}
	if graph.Nodes[1].X <= graph.Nodes[0].X {
		t.Fatalf("dependent job not placed to the right: %+v", graph.Nodes)
	}
}

func TestActionsRunGraphLaysOutWideWorkflowWithoutOverlap(t *testing.T) {
	t.Parallel()
	jobs := make([]actionsJobDetailView, 0, 15)
	for i := range 15 {
		jobs = append(jobs, actionsJobDetailView{
			JobIndex:   int32(i),
			JobKey:     "job-" + strconv.Itoa(i),
			Name:       "Job " + strconv.Itoa(i),
			StateText:  "Queued",
			StateClass: "pending",
			StateIcon:  "dot-fill",
			Duration:   "0s",
			Anchor:     "job-" + strconv.Itoa(i),
		})
	}
	graph := actionsRunGraph(jobs)
	if len(graph.Nodes) != len(jobs) {
		t.Fatalf("nodes=%d, want %d", len(graph.Nodes), len(jobs))
	}
	for i := range graph.Nodes {
		for j := i + 1; j < len(graph.Nodes); j++ {
			if graphNodesOverlap(graph.Nodes[i], graph.Nodes[j]) {
				t.Fatalf("nodes overlap: %+v %+v", graph.Nodes[i], graph.Nodes[j])
			}
		}
	}
	if graph.CanvasHeight < graph.Nodes[len(graph.Nodes)-1].Y+graph.Nodes[len(graph.Nodes)-1].Height+actionsRunGraphMarginY {
		t.Fatalf("canvas does not contain final node: graph=%+v last=%+v", graph, graph.Nodes[len(graph.Nodes)-1])
	}
}

func graphNodesOverlap(a, b actionsRunGraphNodeView) bool {
	return a.X < b.X+b.Width &&
		a.X+a.Width > b.X &&
		a.Y < b.Y+b.Height &&
		a.Y+a.Height > b.Y
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
	testStepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       testID,
		StepIndex:   0,
		RunCommand:  "go test ./...",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		CompletedAt: now.Add(-10 * time.Minute),
	})
	if _, err := actionsdb.New().UpsertWorkflowAnnotation(context.Background(), f.pool, actionsdb.UpsertWorkflowAnnotationParams{
		RunID:       runID,
		JobID:       testID,
		StepID:      testStepID,
		Level:       actionsdb.WorkflowAnnotationLevelWarning,
		Title:       "Slow test",
		Message:     "Use cache",
		Path:        "cmd/main.go",
		StartLine:   pgtype.Int4{Int32: 12, Valid: true},
		LogLine:     pgtype.Int4{Int32: 1, Valid: true},
		LogChunkSeq: pgtype.Int4{Int32: 0, Valid: true},
		Fingerprint: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("UpsertWorkflowAnnotation: %v", err)
	}

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
		"ANNOTATIONS=1:1:0;",
		"AGROUP=Test:1;",
		"ANN=warning:Slow test:Use cache:cmd/main.go:12:/alice/public-repo/blob/trunk/cmd/main.go#L12:/alice/public-repo/actions/runs/7/jobs/1/steps/0;",
		"GRAPH=640x140:2:1;",
		"GNODE=build:32:32:1:0;",
		"GNODE=test:368:32:1:1;",
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

func TestRepoActionRunApprovalControlsAndDecisions(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:       31,
		WorkflowFile:   ".shithub/workflows/pr.yml",
		WorkflowName:   "PR",
		HeadRef:        "refs/heads/contrib",
		Event:          actionsdb.WorkflowRunEventPullRequest,
		Status:         actionsdb.WorkflowRunStatusQueued,
		ActorUserID:    f.stranger.ID,
		CreatedOffset:  -5 * time.Minute,
		NeedApproval:   true,
		ApprovalReason: "Pull request workflow requires maintainer approval before runner dispatch.",
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
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/31", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"RUN=PR:#31:pull_request:bob:pending;",
		"APPROVE=/alice/public-repo/actions/runs/31/approve;",
		"REJECT=/alice/public-repo/actions/runs/31/reject;",
		"APPROVAL_PENDING=Pull request workflow requires maintainer approval before runner dispatch.;",
		"WAIT=Waiting for maintainer approval;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("approval body missing %q in %s", want, body)
		}
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/31", nil)
	f.actionsMux(viewerFor(f.stranger)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("stranger status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "APPROVE=") || strings.Contains(resp.Body.String(), "REJECT=") {
		t.Fatalf("approval controls leaked to stranger: %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/alice/public-repo/actions/runs/31/approve", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("approve status=%d body=%s", resp.Code, resp.Body.String())
	}
	run, err := actionsdb.New().GetWorkflowRunByID(context.Background(), f.pool, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if !run.ApprovedByUserID.Valid || run.ApprovedByUserID.Int64 != f.owner.ID {
		t.Fatalf("approved_by not recorded: %+v", run)
	}

	rejectRunID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:     32,
		WorkflowFile: ".shithub/workflows/pr.yml",
		WorkflowName: "PR",
		HeadRef:      "refs/heads/contrib",
		Event:        actionsdb.WorkflowRunEventPullRequest,
		Status:       actionsdb.WorkflowRunStatusQueued,
		ActorUserID:  f.stranger.ID,
		NeedApproval: true,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:    rejectRunID,
		JobIndex: 0,
		JobKey:   "build",
		JobName:  "Build",
		Status:   actionsdb.WorkflowJobStatusQueued,
	})
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/alice/public-repo/actions/runs/32/reject", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("reject status=%d body=%s", resp.Code, resp.Body.String())
	}
	run, err = actionsdb.New().GetWorkflowRunByID(context.Background(), f.pool, rejectRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID rejected: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusCompleted ||
		!run.Conclusion.Valid || run.Conclusion.CheckConclusion != actionsdb.CheckConclusionActionRequired {
		t.Fatalf("rejected run: %+v", run)
	}
	job, err := actionsdb.New().GetWorkflowJobByID(context.Background(), f.pool, jobID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID rejected: %v", err)
	}
	if job.Status != actionsdb.WorkflowJobStatusCancelled ||
		!job.Conclusion.Valid || job.Conclusion.CheckConclusion != actionsdb.CheckConclusionActionRequired {
		t.Fatalf("rejected job: %+v", job)
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
		"STEPLOG=Build:Run tests:SQL chunks:/alice/public-repo/actions/runs/9/jobs/0/steps/0/log/download:false;",
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
	if _, err := actionsdb.New().UpsertWorkflowAnnotation(context.Background(), f.pool, actionsdb.UpsertWorkflowAnnotationParams{
		RunID:       runID,
		JobID:       jobID,
		StepID:      stepID,
		Level:       actionsdb.WorkflowAnnotationLevelError,
		Title:       "Archive warning",
		Message:     "archived message",
		Path:        ".shithub/workflows/ci.yml",
		StartLine:   pgtype.Int4{Int32: 5, Valid: true},
		Fingerprint: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("UpsertWorkflowAnnotation: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/10/jobs/0/steps/0", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"STEPLOG=Build:Archive:object storage:/alice/public-repo/actions/runs/10/jobs/0/steps/0/log/download:false;",
		"ANN=error:Archive warning:archived message:.shithub/workflows/ci.yml:5:/alice/public-repo/blob/trunk/.shithub/workflows/ci.yml#L5;",
		"LOG=archived\n;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in %s", want, body)
		}
	}
}

func TestRepoActionStepLogDownloadStreamsThroughShithub(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      11,
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
		StepName:   "Run",
		RunCommand: "printf",
		Status:     actionsdb.WorkflowStepStatusRunning,
		StartedAt:  now.Add(-3 * time.Minute),
	})
	f.insertStepLogChunk(t, stepID, 0, "before *** after\n")
	f.insertStepLogChunk(t, stepID, 1, "done\n")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/actions/runs/11/jobs/0/steps/0/log/download", nil)
	f.actionsMux(viewerFor(f.owner)).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Disposition"); got != `attachment; filename="shithub-run-11-job-0-step-0.log"` {
		t.Fatalf("content-disposition=%q", got)
	}
	if got := resp.Body.String(); got != "before *** after\ndone\n" || strings.Contains(got, "hunter2") {
		t.Fatalf("download body=%q", got)
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
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}/log/download", f.handlers.repoActionStepLogDownload)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/jobs/{jobIndex}/steps/{stepIndex}", f.handlers.repoActionStepLog)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}/status", f.handlers.repoActionRunStatus)
	mux.Get("/{owner}/{repo}/actions/runs/{runIndex}", f.handlers.repoActionRun)
	mux.Get("/{owner}/{repo}/actions/workflows/*", f.handlers.repoActionsWorkflow)
	mux.Get("/{owner}/{repo}/actions/caches", f.handlers.repoActionsCaches)
	mux.Get("/{owner}/{repo}/actions/attestations", f.handlers.repoActionsAttestations)
	mux.Get("/{owner}/{repo}/actions/runners", f.handlers.repoActionsRunners)
	mux.Get("/{owner}/{repo}/actions/metrics/usage", f.handlers.repoActionsUsageMetrics)
	mux.Get("/{owner}/{repo}/actions/metrics/performance", f.handlers.repoActionsPerformanceMetrics)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/cancel", f.handlers.repoActionRunCancel)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/rerun", f.handlers.repoActionRunRerun)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/approve", f.handlers.repoActionRunApprove)
	mux.Post("/{owner}/{repo}/actions/runs/{runIndex}/reject", f.handlers.repoActionRunReject)
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
	RunIndex       int64
	WorkflowFile   string
	WorkflowName   string
	HeadSHA        string
	HeadRef        string
	Event          actionsdb.WorkflowRunEvent
	EventPayload   string
	Status         actionsdb.WorkflowRunStatus
	Conclusion     actionsdb.CheckConclusion
	ActorUserID    int64
	CreatedOffset  time.Duration
	StartedOffset  time.Duration
	DoneOffset     time.Duration
	RepoID         int64
	NeedApproval   bool
	ApprovalReason string
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
			status, conclusion, need_approval, started_at, completed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8::jsonb, $9,
			$10, $11, $12, $13, $14, $15, $16
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
		fx.NeedApproval,
		startedAt,
		completedAt,
		createdAt,
		createdAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workflow run %d: %v", fx.RunIndex, err)
	}
	if fx.NeedApproval {
		reason := fx.ApprovalReason
		if reason == "" {
			reason = "approval required"
		}
		if _, err := actionsdb.New().InsertWorkflowRunApproval(context.Background(), f.pool, actionsdb.InsertWorkflowRunApprovalParams{
			RunID:           id,
			RequestedReason: reason,
		}); err != nil {
			t.Fatalf("insert workflow approval %d: %v", fx.RunIndex, err)
		}
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
