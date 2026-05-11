// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runner orchestrates the shithubd-runner claim/execute/status loop.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tenseleyFlow/shithub/internal/runner/api"
	"github.com/tenseleyFlow/shithub/internal/runner/engine"
)

type API interface {
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) (*api.Claim, error)
	UpdateStatus(ctx context.Context, jobID int64, token string, req api.StatusRequest) (api.StatusResponse, error)
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
	token := claim.Token
	started := r.clock()
	workspaceDir, err := r.workspaces.Prepare(claim.Job.RunID, claim.Job.ID)
	if err != nil {
		statusErr := r.complete(ctx, claim.Job.ID, token, engine.ConclusionFailure, started, r.clock())
		return true, errors.Join(fmt.Errorf("prepare workspace: %w", err), statusErr)
	}
	defer func() {
		if err := r.workspaces.Remove(claim.Job.RunID, claim.Job.ID); err != nil {
			r.logger.WarnContext(ctx, "workspace cleanup failed", "run_id", claim.Job.RunID, "job_id", claim.Job.ID, "error", err)
		}
	}()

	running, err := r.api.UpdateStatus(ctx, claim.Job.ID, token, api.StatusRequest{
		Status:    "running",
		StartedAt: started,
	})
	if err != nil {
		return true, fmt.Errorf("mark job running: %w", err)
	}
	if running.NextToken == "" {
		return true, errors.New("mark job running: server did not return next_token")
	}
	token = running.NextToken

	outcome, execErr := r.engine.Execute(ctx, toEngineJob(claim.Job, workspaceDir, r.defaultImage))
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
	if err := r.complete(ctx, claim.Job.ID, token, conclusion, outcome.StartedAt, completed); err != nil {
		return true, err
	}
	if execErr != nil {
		r.logger.WarnContext(ctx, "job completed with failing engine outcome", "job_id", claim.Job.ID, "conclusion", conclusion, "error", execErr)
	}
	return true, nil
}

func (r *Runner) complete(ctx context.Context, jobID int64, token, conclusion string, started, completed time.Time) error {
	_, err := r.api.UpdateStatus(ctx, jobID, token, api.StatusRequest{
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
