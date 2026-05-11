// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
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

func (f *repoFixture) actionsMux(viewer middleware.CurrentUser) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
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
}

func (f *repoFixture) insertWorkflowRun(t *testing.T, fx workflowRunFixture, base time.Time) {
	t.Helper()
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
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO workflow_runs (
			repo_id, run_index, workflow_file, workflow_name,
			head_sha, head_ref, event, event_payload, actor_user_id,
			status, conclusion, started_at, completed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, '{}'::jsonb, $8,
			$9, $10, $11, $12, $13, $14
		)`,
		f.publicRepo.ID,
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
	)
	if err != nil {
		t.Fatalf("insert workflow run %d: %v", fx.RunIndex, err)
	}
}

func strconvDigit(n int64) string {
	return strconv.FormatInt(n%10, 10)
}
