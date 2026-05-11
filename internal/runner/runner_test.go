// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/runner/api"
	"github.com/tenseleyFlow/shithub/internal/runner/engine"
)

type fakeAPI struct {
	claim        *api.Claim
	statuses     []api.StatusRequest
	stepStatuses []api.StatusRequest
	logs         []api.LogRequest
	tokens       []string
	next         int
}

func (f *fakeAPI) Heartbeat(_ context.Context, _ api.HeartbeatRequest) (*api.Claim, error) {
	return f.claim, nil
}

func (f *fakeAPI) UpdateStatus(_ context.Context, _ int64, token string, req api.StatusRequest) (api.StatusResponse, error) {
	f.tokens = append(f.tokens, token)
	f.statuses = append(f.statuses, req)
	if req.Status == "running" {
		return api.StatusResponse{NextToken: f.nextToken()}, nil
	}
	return api.StatusResponse{}, nil
}

func (f *fakeAPI) UpdateStepStatus(_ context.Context, _, _ int64, token string, req api.StatusRequest) (api.StepStatusResponse, error) {
	f.tokens = append(f.tokens, token)
	f.stepStatuses = append(f.stepStatuses, req)
	return api.StepStatusResponse{NextToken: f.nextToken()}, nil
}

func (f *fakeAPI) AppendLog(_ context.Context, _ int64, token string, req api.LogRequest) (api.LogResponse, error) {
	f.tokens = append(f.tokens, token)
	f.logs = append(f.logs, req)
	return api.LogResponse{NextToken: f.nextToken()}, nil
}

func (f *fakeAPI) nextToken() string {
	f.next++
	return "next-token-" + strconv.Itoa(f.next)
}

type fakeEngine struct {
	job  engine.Job
	out  engine.Outcome
	logs []engine.LogChunk
	err  error
}

func (f *fakeEngine) Execute(_ context.Context, job engine.Job) (engine.Outcome, error) {
	f.job = job
	return f.out, f.err
}

func (f *fakeEngine) StreamLogs(_ context.Context, _ int64) (<-chan engine.LogChunk, error) {
	ch := make(chan engine.LogChunk, len(f.logs))
	for _, chunk := range f.logs {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func (f *fakeEngine) Cancel(_ context.Context, _ int64) error { return nil }

type fakeEventEngine struct {
	fakeEngine
	events []engine.Event
	ch     chan engine.Event
}

func (f *fakeEventEngine) StreamEvents(_ context.Context, _ int64) (<-chan engine.Event, error) {
	f.ch = make(chan engine.Event, len(f.events))
	return f.ch, nil
}

func (f *fakeEventEngine) Execute(ctx context.Context, job engine.Job) (engine.Outcome, error) {
	out, err := f.fakeEngine.Execute(ctx, job)
	for _, event := range f.events {
		f.ch <- event
	}
	close(f.ch)
	return out, err
}

type fakeWorkspaces struct {
	dir     string
	removed bool
	err     error
}

func (f *fakeWorkspaces) Prepare(_, _ int64) (string, error) {
	return f.dir, f.err
}

func (f *fakeWorkspaces) Remove(_, _ int64) error {
	f.removed = true
	return nil
}

func TestRunOnce_NoClaim(t *testing.T) {
	t.Parallel()
	r := New(Options{API: &fakeAPI{}, Engine: &fakeEngine{}, Workspaces: &fakeWorkspaces{}})
	claimed, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if claimed {
		t.Fatal("claimed = true")
	}
}

func TestRunOnce_ExecutesAndCompletesSuccess(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	claim := &api.Claim{
		Token: "job-token",
		Job: api.Job{
			ID:    10,
			RunID: 20,
			Env:   map[string]string{"A": "B"},
			Steps: []api.Step{{ID: 30, Run: "echo hi"}},
		},
	}
	fapi := &fakeAPI{claim: claim}
	fengine := &fakeEngine{out: engine.Outcome{Conclusion: engine.ConclusionSuccess, StartedAt: now, CompletedAt: now.Add(time.Second)}}
	workspaces := &fakeWorkspaces{dir: "/tmp/workspace"}
	r := New(Options{
		API:          fapi,
		Engine:       fengine,
		Workspaces:   workspaces,
		DefaultImage: "runner-image",
		Clock:        func() time.Time { return now },
	})
	claimed, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !claimed {
		t.Fatal("claimed = false")
	}
	if fengine.job.WorkspaceDir != "/tmp/workspace" || fengine.job.Image != "runner-image" {
		t.Fatalf("engine job: %#v", fengine.job)
	}
	if len(fapi.statuses) != 2 {
		t.Fatalf("statuses: %#v", fapi.statuses)
	}
	if fapi.statuses[0].Status != "running" || fapi.statuses[1].Conclusion != engine.ConclusionSuccess {
		t.Fatalf("statuses: %#v", fapi.statuses)
	}
	if len(fapi.tokens) != 2 || fapi.tokens[0] != "job-token" || fapi.tokens[1] != "next-token-1" {
		t.Fatalf("tokens: %#v", fapi.tokens)
	}
	if !workspaces.removed {
		t.Fatal("workspace was not removed")
	}
}

func TestRunOnce_StreamsLogsAndMarksSteps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	claim := &api.Claim{
		Token: "job-token",
		Job: api.Job{
			ID:    10,
			RunID: 20,
			Steps: []api.Step{{ID: 30, Run: "echo hi"}},
		},
	}
	fapi := &fakeAPI{claim: claim}
	fengine := &fakeEngine{
		out: engine.Outcome{
			Conclusion:  engine.ConclusionSuccess,
			StartedAt:   now,
			CompletedAt: now.Add(time.Second),
			StepOutcomes: []engine.StepOutcome{{
				StepID:      30,
				Status:      "completed",
				Conclusion:  engine.ConclusionSuccess,
				StartedAt:   now,
				CompletedAt: now.Add(500 * time.Millisecond),
			}},
		},
		logs: []engine.LogChunk{{
			JobID:  10,
			StepID: 30,
			Seq:    0,
			Chunk:  []byte("hello\n"),
		}},
	}
	r := New(Options{
		API:        fapi,
		Engine:     fengine,
		Workspaces: &fakeWorkspaces{dir: "/tmp/workspace"},
		Clock:      func() time.Time { return now },
	})
	claimed, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !claimed {
		t.Fatal("claimed = false")
	}
	if len(fapi.logs) != 1 || fapi.logs[0].StepID != 30 || string(fapi.logs[0].Chunk) != "hello\n" {
		t.Fatalf("logs: %#v", fapi.logs)
	}
	if len(fapi.stepStatuses) != 1 || fapi.stepStatuses[0].Status != "completed" ||
		fapi.stepStatuses[0].Conclusion != engine.ConclusionSuccess {
		t.Fatalf("step statuses: %#v", fapi.stepStatuses)
	}
	wantTokens := []string{"job-token", "next-token-1", "next-token-2", "next-token-3"}
	if len(fapi.tokens) != len(wantTokens) {
		t.Fatalf("tokens: got %#v, want %#v", fapi.tokens, wantTokens)
	}
	for i, want := range wantTokens {
		if fapi.tokens[i] != want {
			t.Fatalf("tokens[%d]: got %q, want %q; all=%#v", i, fapi.tokens[i], want, fapi.tokens)
		}
	}
}

func TestRunOnce_PrefersOrderedEventStream(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	logEvent := engine.LogChunk{JobID: 10, StepID: 30, Seq: 0, Chunk: []byte("hello\n")}
	stepEvent := engine.StepOutcome{
		StepID:      30,
		Status:      "completed",
		Conclusion:  engine.ConclusionSuccess,
		StartedAt:   now,
		CompletedAt: now.Add(500 * time.Millisecond),
	}
	fapi := &fakeAPI{claim: &api.Claim{
		Token: "job-token",
		Job:   api.Job{ID: 10, RunID: 20, Steps: []api.Step{{ID: 30, Run: "echo hi"}}},
	}}
	fengine := &fakeEventEngine{
		fakeEngine: fakeEngine{out: engine.Outcome{
			Conclusion:   engine.ConclusionSuccess,
			StartedAt:    now,
			CompletedAt:  now.Add(time.Second),
			StepOutcomes: []engine.StepOutcome{stepEvent},
		}},
		events: []engine.Event{{Log: &logEvent}, {Step: &stepEvent}},
	}
	r := New(Options{
		API:        fapi,
		Engine:     fengine,
		Workspaces: &fakeWorkspaces{dir: "/tmp/workspace"},
		Clock:      func() time.Time { return now },
	})
	if _, err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(fapi.logs) != 1 || len(fapi.stepStatuses) != 1 {
		t.Fatalf("logs=%#v stepStatuses=%#v", fapi.logs, fapi.stepStatuses)
	}
	wantTokens := []string{"job-token", "next-token-1", "next-token-2", "next-token-3"}
	if len(fapi.tokens) != len(wantTokens) {
		t.Fatalf("tokens: got %#v, want %#v", fapi.tokens, wantTokens)
	}
	for i, want := range wantTokens {
		if fapi.tokens[i] != want {
			t.Fatalf("tokens[%d]: got %q, want %q; all=%#v", i, fapi.tokens[i], want, fapi.tokens)
		}
	}
}

func TestRunOnce_EngineFailureStillCompletesJob(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	fapi := &fakeAPI{claim: &api.Claim{Token: "job-token", Job: api.Job{ID: 10, RunID: 20}}}
	fengine := &fakeEngine{
		out: engine.Outcome{Conclusion: engine.ConclusionFailure, StartedAt: now, CompletedAt: now.Add(time.Second)},
		err: errors.New("exit 1"),
	}
	r := New(Options{
		API:        fapi,
		Engine:     fengine,
		Workspaces: &fakeWorkspaces{dir: "/tmp/workspace"},
		Clock:      func() time.Time { return now },
	})
	if _, err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fapi.statuses[1].Conclusion != engine.ConclusionFailure {
		t.Fatalf("completion: %#v", fapi.statuses[1])
	}
}

func TestRunOnce_PrepareFailureMarksJobFailed(t *testing.T) {
	t.Parallel()
	fapi := &fakeAPI{claim: &api.Claim{Token: "job-token", Job: api.Job{ID: 10, RunID: 20}}}
	r := New(Options{
		API:        fapi,
		Engine:     &fakeEngine{},
		Workspaces: &fakeWorkspaces{err: errors.New("disk full")},
		Clock:      func() time.Time { return time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC) },
	})
	claimed, err := r.RunOnce(t.Context())
	if err == nil {
		t.Fatal("RunOnce returned nil error")
	}
	if !claimed {
		t.Fatal("claimed = false")
	}
	if len(fapi.statuses) != 1 || fapi.statuses[0].Conclusion != engine.ConclusionFailure {
		t.Fatalf("statuses: %#v", fapi.statuses)
	}
}
