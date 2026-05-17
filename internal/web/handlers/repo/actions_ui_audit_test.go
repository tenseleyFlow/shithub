// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestActionsProductionTemplatesRenderParityLandmarks(t *testing.T) {
	t.Parallel()
	f := newActionsUIAuditFixture(t)
	ids := f.seedActionsUIAuditData(t, 41, "CI")
	f.insertWorkflowCache(t, f.publicRepo.ID, "go-build", "v1", "refs/heads/trunk", 2048)
	f.insertWorkflowRunnerWith(t, "shared-linux", []string{"self-hosted", "linux", "ubuntu-latest"}, actionsdb.WorkflowRunnerStatusIdle, 2, time.Now().UTC())
	f.seedWorkflowFile(t, "manual.yml", dispatchWorkflowFixture)

	mux := f.actionsMux(viewerFor(f.owner))
	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "actions list",
			path: "/alice/public-repo/actions",
			want: []string{
				`<section class="shithub-actions-page">`,
				`aria-label="Actions navigation"`,
				`New workflow`,
				`Run workflow`,
				`aria-label="Workflow run filters"`,
				`placeholder="Filter workflow runs"`,
				`aria-label="Workflow runs"`,
				`/alice/public-repo/actions/caches`,
				`/alice/public-repo/actions/metrics/performance`,
			},
		},
		{
			name: "workflow route",
			path: "/alice/public-repo/actions/workflows/ci.yml",
			want: []string{
				`<h1>CI</h1>`,
				`href="/alice/public-repo/blob/trunk/.shithub/workflows/ci.yml"`,
				`class="shithub-actions-run-row"`,
				`Workflow`,
				`Status`,
				`Branch`,
				`Actor`,
			},
		},
		{
			name: "new workflow picker",
			path: "/alice/public-repo/actions/new",
			want: []string{
				`class="shithub-actions-page shithub-actions-new-page"`,
				`<h1>New workflow</h1>`,
				`Starter workflows`,
				`Minimal shell smoke`,
				`href="/alice/public-repo/actions/new?template=smoke"`,
				`GitHub templates not offered yet`,
				`Matrix build`,
			},
		},
		{
			name: "run summary graph",
			path: "/alice/public-repo/actions/runs/41",
			want: []string{
				`class="shithub-actions-run-page"`,
				`aria-label="Run navigation"`,
				`data-actions-graph-shell`,
				`data-actions-graph-viewport`,
				`aria-label="Interactive workflow job graph"`,
				`aria-label="Workflow graph controls"`,
				`data-actions-graph-popover`,
				`id="annotations"`,
				`aria-label="Workflow jobs"`,
				`Re-run jobs`,
			},
		},
		{
			name: "step log",
			path: "/alice/public-repo/actions/runs/41/jobs/0/steps/0",
			want: []string{
				`shithub-actions-log-page`,
				`href="/alice/public-repo/actions/runs/41/jobs/0/steps/0/log/download"`,
				`Download`,
				`SQL chunks`,
				`<pre class="shithub-actions-log-output"><code>`,
				`checkout complete`,
			},
		},
		{
			name: "caches",
			path: "/alice/public-repo/actions/caches",
			want: []string{
				`<h1>Caches</h1>`,
				`Showing caches from all workflows`,
				`go-build`,
				`v1`,
			},
		},
		{
			name: "attestations",
			path: "/alice/public-repo/actions/attestations",
			want: []string{
				`<h1>Attestations</h1>`,
				`Search or filter`,
				`No attestations`,
			},
		},
		{
			name: "runners",
			path: "/alice/public-repo/actions/runners",
			want: []string{
				`<h1>Runners</h1>`,
				`shithub-hosted runners`,
				`Self-hosted runners`,
				`shared-linux`,
				`ubuntu-latest`,
			},
		},
		{
			name: "usage metrics",
			path: "/alice/public-repo/actions/metrics/usage",
			want: []string{
				`<h1>Actions Usage Metrics</h1>`,
				`Total minutes`,
				`Workflow runs`,
				`CI`,
			},
		},
		{
			name: "performance metrics",
			path: "/alice/public-repo/actions/metrics/performance",
			want: []string{
				`<h1>Actions Performance Metrics</h1>`,
				`Avg job run time`,
				`Failure rate`,
				`CI`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			mux.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			assertContainsAll(t, resp.Body.String(), tt.want)
		})
	}

	if ids.runID == 0 || ids.jobID == 0 || ids.stepID == 0 {
		t.Fatalf("audit fixture did not create stable ids: %+v", ids)
	}
}

func TestActionsProductionTemplatesEscapeRunAnnotationsAndLogs(t *testing.T) {
	t.Parallel()
	f := newActionsUIAuditFixture(t)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      42,
		WorkflowFile:  ".shithub/workflows/pwn.yml",
		WorkflowName:  `<script>alert("workflow")</script>`,
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionFailure,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -10 * time.Minute,
		StartedOffset: -9 * time.Minute,
		DoneOffset:    -8 * time.Minute,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:       runID,
		JobIndex:    0,
		JobKey:      "pwn",
		JobName:     `<img src=x onerror=alert(1)>`,
		RunsOn:      "ubuntu-latest",
		Status:      actionsdb.WorkflowJobStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now.Add(-8 * time.Minute),
	})
	stepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       jobID,
		StepIndex:   0,
		StepName:    `<svg onload=alert(1)>`,
		RunCommand:  `printf "masked"`,
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now.Add(-8 * time.Minute),
	})
	f.insertStepLogChunk(t, stepID, 0, "before *** after\n<svg onload=alert(1)>\n")
	if _, err := actionsdb.New().UpsertWorkflowAnnotation(context.Background(), f.pool, actionsdb.UpsertWorkflowAnnotationParams{
		RunID:       runID,
		JobID:       jobID,
		StepID:      stepID,
		Level:       actionsdb.WorkflowAnnotationLevelError,
		Title:       `<script>alert("annotation")</script>`,
		Message:     `<img src=x onerror=alert(1)>`,
		Path:        "README.md",
		StartLine:   pgtype.Int4{Int32: 1, Valid: true},
		LogLine:     pgtype.Int4{Int32: 1, Valid: true},
		LogChunkSeq: pgtype.Int4{Int32: 0, Valid: true},
		Fingerprint: strings.Repeat("c", 64),
	}); err != nil {
		t.Fatalf("UpsertWorkflowAnnotation: %v", err)
	}

	mux := f.actionsMux(viewerFor(f.owner))
	runBody := mustGetActionsAuditBody(t, mux, "/alice/public-repo/actions/runs/42")
	logBody := mustGetActionsAuditBody(t, mux, "/alice/public-repo/actions/runs/42/jobs/0/steps/0")
	combined := runBody + "\n" + logBody

	for _, raw := range []string{
		`<script>alert("workflow")</script>`,
		`<script>alert("annotation")</script>`,
		`<img src=x onerror=alert(1)>`,
		`<svg onload=alert(1)>`,
	} {
		if strings.Contains(combined, raw) {
			t.Fatalf("raw executable markup leaked: %q in\n%s", raw, combined)
		}
	}
	assertContainsAll(t, combined, []string{
		`&lt;script&gt;alert(&#34;workflow&#34;)&lt;/script&gt;`,
		`&lt;script&gt;alert(&#34;annotation&#34;)&lt;/script&gt;`,
		`&lt;img src=x onerror=alert(1)&gt;`,
		`&lt;svg onload=alert(1)&gt;`,
		`before *** after`,
		`href="/alice/public-repo/actions/runs/42/jobs/0/steps/0/log/download"`,
	})
	for _, leak := range []string{"X-Amz-Signature", "digitaloceanspaces.com", "LogObjectKey"} {
		if strings.Contains(logBody, leak) {
			t.Fatalf("step log page leaked storage detail %q in\n%s", leak, logBody)
		}
	}
}

func TestActionsProductionTemplatesUseRegisteredOcticons(t *testing.T) {
	t.Parallel()
	files := []string{
		"../../templates/repo/_actions_sidebar.html",
		"../../templates/repo/actions.html",
		"../../templates/repo/action_run.html",
		"../../templates/repo/action_step_log.html",
		"../../templates/repo/actions_management.html",
	}
	iconRE := regexp.MustCompile(`octicon\s+"([^"]+)"`)
	names := map[string]bool{
		// Dynamic icon names used by Actions view models.
		"cache":        true,
		"clock":        true,
		"dot-fill":     true,
		"repo":         true,
		"server":       true,
		"shield-check": true,
		"stopwatch":    true,
		"workflow":     true,
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, match := range iconRE.FindAllStringSubmatch(string(body), -1) {
			names[match[1]] = true
		}
	}

	resolver := render.BuiltinOcticons()
	var missing []string
	for name := range names {
		if _, ok := resolver(name); !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("Actions templates reference unregistered octicons: %s", strings.Join(missing, ", "))
	}
}

func TestActionsRunGraphLaysOutHundredJobWorkflowWithoutOverlap(t *testing.T) {
	t.Parallel()
	jobs := make([]actionsJobDetailView, 0, 100)
	for i := range 100 {
		jobs = append(jobs, actionsJobDetailView{
			JobIndex:   int32(i),
			JobKey:     "job-" + strconv.Itoa(i),
			Name:       "Long parallel job name " + strconv.Itoa(i),
			RunsOn:     "ubuntu-latest",
			StateText:  "Success",
			StateClass: "success",
			StateIcon:  "check-circle",
			Duration:   strconv.Itoa(i%10+1) + "s",
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
	last := graph.Nodes[len(graph.Nodes)-1]
	if graph.CanvasHeight < last.Y+last.Height+actionsRunGraphMarginY {
		t.Fatalf("canvas does not contain final node: graph=%+v last=%+v", graph, last)
	}
}

type actionsUIAuditIDs struct {
	runID  int64
	jobID  int64
	stepID int64
}

func newActionsUIAuditFixture(t *testing.T) *repoFixture {
	t.Helper()
	return newRepoFixtureWithTemplates(t, os.DirFS("../../templates"), render.Options{Octicons: render.BuiltinOcticons()})
}

func (f *repoFixture) seedActionsUIAuditData(t *testing.T, runIndex int64, workflowName string) actionsUIAuditIDs {
	t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	runID := f.insertWorkflowRun(t, workflowRunFixture{
		RunIndex:      runIndex,
		WorkflowFile:  ".shithub/workflows/ci.yml",
		WorkflowName:  workflowName,
		HeadRef:       "trunk",
		Event:         actionsdb.WorkflowRunEventPush,
		Status:        actionsdb.WorkflowRunStatusCompleted,
		Conclusion:    actionsdb.CheckConclusionFailure,
		ActorUserID:   f.owner.ID,
		CreatedOffset: -10 * time.Minute,
		StartedOffset: -9 * time.Minute,
		DoneOffset:    -8 * time.Minute,
	}, now)
	jobID := f.insertWorkflowJob(t, workflowJobFixture{
		RunID:       runID,
		JobIndex:    0,
		JobKey:      "green",
		JobName:     "green",
		RunsOn:      "ubuntu-latest",
		Status:      actionsdb.WorkflowJobStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now.Add(-8 * time.Minute),
	})
	stepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       jobID,
		StepIndex:   0,
		StepName:    "Checkout",
		UsesAlias:   "actions/checkout@v4",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionSuccess,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now.Add(-8 * time.Minute),
	})
	f.insertStepLogChunk(t, stepID, 0, "checkout complete\n")
	verifyStepID := f.insertWorkflowStep(t, workflowStepFixture{
		JobID:       jobID,
		StepIndex:   1,
		StepName:    "Verify",
		RunCommand:  "go test ./...",
		Status:      actionsdb.WorkflowStepStatusCompleted,
		Conclusion:  actionsdb.CheckConclusionFailure,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now.Add(-8 * time.Minute),
	})
	if _, err := actionsdb.New().UpsertWorkflowAnnotation(context.Background(), f.pool, actionsdb.UpsertWorkflowAnnotationParams{
		RunID:       runID,
		JobID:       jobID,
		StepID:      verifyStepID,
		Level:       actionsdb.WorkflowAnnotationLevelWarning,
		Title:       "Test warning",
		Message:     "Use a smaller fixture",
		Path:        "README.md",
		StartLine:   pgtype.Int4{Int32: 3, Valid: true},
		LogLine:     pgtype.Int4{Int32: 2, Valid: true},
		LogChunkSeq: pgtype.Int4{Int32: 0, Valid: true},
		Fingerprint: strings.Repeat("d", 64),
	}); err != nil {
		t.Fatalf("UpsertWorkflowAnnotation: %v", err)
	}
	return actionsUIAuditIDs{runID: runID, jobID: jobID, stepID: stepID}
}

func mustGetActionsAuditBody(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
	return resp.Body.String()
}

func assertContainsAll(t *testing.T, body string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q in\n%s", want, body)
		}
	}
}
