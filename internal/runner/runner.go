// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runner orchestrates the shithubd-runner claim/execute/status loop.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tenseleyFlow/shithub/internal/runner/api"
	"github.com/tenseleyFlow/shithub/internal/runner/engine"
)

type API interface {
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) (*api.Claim, error)
	UpdateStatus(ctx context.Context, jobID int64, token string, req api.StatusRequest) (api.StatusResponse, error)
	UpdateStepStatus(ctx context.Context, jobID, stepID int64, token string, req api.StatusRequest) (api.StepStatusResponse, error)
	AppendLog(ctx context.Context, jobID int64, token string, req api.LogRequest) (api.LogResponse, error)
}

type Workspaces interface {
	Prepare(runID, jobID int64) (string, error)
	Remove(runID, jobID int64) error
}

type SleepFunc func(ctx context.Context, d time.Duration) error

type Options struct {
	API          API
	Engine       engine.Engine
	Workspaces   Workspaces
	Logger       *slog.Logger
	Labels       []string
	Capacity     int
	PollInterval time.Duration
	DefaultImage string
	Clock        func() time.Time
	Sleep        SleepFunc
}

type Runner struct {
	api          API
	engine       engine.Engine
	workspaces   Workspaces
	logger       *slog.Logger
	labels       []string
	capacity     int
	pollInterval time.Duration
	defaultImage string
	clock        func() time.Time
	sleep        SleepFunc
}

func New(opts Options) *Runner {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = defaultSleep
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	capacity := opts.Capacity
	if capacity <= 0 {
		capacity = 1
	}
	return &Runner{
		api:          opts.API,
		engine:       opts.Engine,
		workspaces:   opts.Workspaces,
		logger:       logger,
		labels:       append([]string{}, opts.Labels...),
		capacity:     capacity,
		pollInterval: poll,
		defaultImage: opts.DefaultImage,
		clock:        clock,
		sleep:        sleep,
	}
}

func (r *Runner) Run(ctx context.Context) error {
	for {
		claimed, err := r.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			r.logger.ErrorContext(ctx, "runner loop iteration failed", "error", err)
		}
		if claimed {
			continue
		}
		if err := r.sleep(ctx, r.pollInterval); err != nil {
			return err
		}
	}
}

func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	claim, err := r.api.Heartbeat(ctx, api.HeartbeatRequest{Labels: r.labels, Capacity: r.capacity})
	if err != nil {
		return false, err
	}
	if claim == nil {
		return false, nil
	}
	session := newJobSession(r.api, claim.Job.ID, claim.Token)
	started := r.clock()
	workspaceDir, err := r.workspaces.Prepare(claim.Job.RunID, claim.Job.ID)
	if err != nil {
		statusErr := r.complete(ctx, session, engine.ConclusionFailure, started, r.clock())
		return true, errors.Join(fmt.Errorf("prepare workspace: %w", err), statusErr)
	}
	defer func() {
		if err := r.workspaces.Remove(claim.Job.RunID, claim.Job.ID); err != nil {
			r.logger.WarnContext(ctx, "workspace cleanup failed", "run_id", claim.Job.RunID, "job_id", claim.Job.ID, "error", err)
		}
	}()

	running, err := session.UpdateStatus(ctx, api.StatusRequest{
		Status:    "running",
		StartedAt: started,
	})
	if err != nil {
		return true, fmt.Errorf("mark job running: %w", err)
	}
	if running.NextToken == "" {
		return true, errors.New("mark job running: server did not return next_token")
	}
	var (
		streamedEvents bool
		drainErr       chan error
	)
	if streamer, ok := r.engine.(engine.EventStreamer); ok {
		events, err := streamer.StreamEvents(ctx, claim.Job.ID)
		if err != nil {
			return true, fmt.Errorf("open event stream: %w", err)
		}
		streamedEvents = true
		drainErr = make(chan error, 1)
		go func() {
			drainErr <- drainEvents(ctx, session, events)
		}()
	} else {
		logs, err := r.engine.StreamLogs(ctx, claim.Job.ID)
		if err != nil {
			return true, fmt.Errorf("open log stream: %w", err)
		}
		drainErr = make(chan error, 1)
		go func() {
			drainErr <- drainLogs(ctx, session, logs)
		}()
	}

	outcome, execErr := r.engine.Execute(ctx, toEngineJob(claim.Job, workspaceDir, r.defaultImage))
	if err := <-drainErr; err != nil {
		return true, fmt.Errorf("stream runner events: %w", err)
	}
	if !streamedEvents {
		for _, step := range outcome.StepOutcomes {
			if step.StepID == 0 {
				continue
			}
			if err := session.UpdateStepStatus(ctx, step.StepID, api.StatusRequest{
				Status:      step.Status,
				Conclusion:  step.Conclusion,
				StartedAt:   step.StartedAt,
				CompletedAt: step.CompletedAt,
			}); err != nil {
				return true, fmt.Errorf("mark step completed: %w", err)
			}
		}
	}
	conclusion := outcome.Conclusion
	if conclusion == "" {
		conclusion = engine.ConclusionFailure
	}
	completed := outcome.CompletedAt
	if completed.IsZero() {
		completed = r.clock()
	}
	if outcome.StartedAt.IsZero() {
		outcome.StartedAt = started
	}
	if err := r.complete(ctx, session, conclusion, outcome.StartedAt, completed); err != nil {
		return true, err
	}
	if execErr != nil {
		r.logger.WarnContext(ctx, "job completed with failing engine outcome", "job_id", claim.Job.ID, "conclusion", conclusion, "error", execErr)
	}
	return true, nil
}

func (r *Runner) complete(ctx context.Context, session *jobSession, conclusion string, started, completed time.Time) error {
	_, err := session.UpdateStatus(ctx, api.StatusRequest{
		Status:      "completed",
		Conclusion:  conclusion,
		StartedAt:   started,
		CompletedAt: completed,
	})
	if err != nil {
		return fmt.Errorf("mark job completed: %w", err)
	}
	return nil
}

type jobSession struct {
	api   API
	jobID int64
	token string
	mu    sync.Mutex
}

func newJobSession(api API, jobID int64, token string) *jobSession {
	return &jobSession{api: api, jobID: jobID, token: token}
}

func (s *jobSession) UpdateStatus(ctx context.Context, req api.StatusRequest) (api.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp, err := s.api.UpdateStatus(ctx, s.jobID, s.token, req)
	if err != nil {
		return resp, err
	}
	if resp.NextToken != "" {
		s.token = resp.NextToken
	}
	return resp, nil
}

func (s *jobSession) UpdateStepStatus(ctx context.Context, stepID int64, req api.StatusRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp, err := s.api.UpdateStepStatus(ctx, s.jobID, stepID, s.token, req)
	if err != nil {
		return err
	}
	if resp.NextToken != "" {
		s.token = resp.NextToken
	}
	return nil
}

func (s *jobSession) AppendLog(ctx context.Context, chunk engine.LogChunk) error {
	if len(chunk.Chunk) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp, err := s.api.AppendLog(ctx, s.jobID, s.token, api.LogRequest{
		Seq:    chunk.Seq,
		Chunk:  chunk.Chunk,
		StepID: chunk.StepID,
	})
	if err != nil {
		return err
	}
	if resp.NextToken != "" {
		s.token = resp.NextToken
	}
	return nil
}

func drainLogs(ctx context.Context, session *jobSession, logs <-chan engine.LogChunk) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-logs:
			if !ok {
				return nil
			}
			if err := session.AppendLog(ctx, chunk); err != nil {
				return err
			}
		}
	}
}

func drainEvents(ctx context.Context, session *jobSession, events <-chan engine.Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.Log != nil {
				if err := session.AppendLog(ctx, *event.Log); err != nil {
					return err
				}
			}
			if event.Step != nil && event.Step.StepID != 0 {
				if err := session.UpdateStepStatus(ctx, event.Step.StepID, api.StatusRequest{
					Status:      event.Step.Status,
					Conclusion:  event.Step.Conclusion,
					StartedAt:   event.Step.StartedAt,
					CompletedAt: event.Step.CompletedAt,
				}); err != nil {
					return err
				}
			}
		}
	}
}

func toEngineJob(job api.Job, workspaceDir, defaultImage string) engine.Job {
	steps := make([]engine.Step, 0, len(job.Steps))
	for _, step := range job.Steps {
		steps = append(steps, engine.Step{
			ID:               step.ID,
			Index:            step.Index,
			StepID:           step.StepID,
			Name:             step.Name,
			If:               step.If,
			Run:              step.Run,
			Uses:             step.Uses,
			WorkingDirectory: step.WorkingDirectory,
			Env:              step.Env,
			With:             step.With,
			ContinueOnError:  step.ContinueOnError,
		})
	}
	return engine.Job{
		ID:             job.ID,
		RunID:          job.RunID,
		RepoID:         job.RepoID,
		RunIndex:       job.RunIndex,
		WorkflowFile:   job.WorkflowFile,
		WorkflowName:   job.WorkflowName,
		HeadSHA:        job.HeadSHA,
		HeadRef:        job.HeadRef,
		Event:          job.Event,
		EventPayload:   job.EventPayload,
		JobKey:         job.JobKey,
		JobName:        job.JobName,
		RunsOn:         job.RunsOn,
		Needs:          append([]string{}, job.Needs...),
		If:             job.If,
		TimeoutMinutes: job.TimeoutMinutes,
		Permissions:    job.Permissions,
		Env:            job.Env,
		Steps:          steps,
		WorkspaceDir:   workspaceDir,
		Image:          defaultImage,
	}
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
