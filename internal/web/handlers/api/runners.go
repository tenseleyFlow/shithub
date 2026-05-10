// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/runnerlabels"
	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
)

var runnerHeartbeatLimit = ratelimit.Policy{
	Scope:  "actions:runner_dispatch",
	Max:    60,
	Window: time.Minute,
}

func (h *Handlers) mountRunners(r chi.Router) {
	r.Post("/api/v1/runners/heartbeat", h.runnerHeartbeat)
}

type runnerHeartbeatRequest struct {
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
}

func (h *Handlers) runnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if h.d.RunnerJWT == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runner API is not configured")
		return
	}
	runner, ok := h.authenticateRunner(w, r)
	if !ok {
		return
	}
	if !h.allowRunnerHeartbeat(w, r, runner.ID) {
		return
	}

	var body runnerHeartbeatRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	labels := runner.Labels
	if body.Labels != nil {
		var err error
		labels, err = runnerlabels.Normalize(body.Labels)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	capacity := int(runner.Capacity)
	if body.Capacity != 0 {
		capacity = body.Capacity
	}
	if capacity < 1 || capacity > 64 {
		writeAPIError(w, http.StatusBadRequest, "capacity must be between 1 and 64")
		return
	}

	job, steps, claimed, err := h.claimRunnerJob(r.Context(), runner.ID, labels, int32(capacity))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner heartbeat claim failed", "runner_id", runner.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner heartbeat failed")
		return
	}
	if !claimed {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	token, claims, err := h.d.RunnerJWT.Mint(runnerjwt.MintParams{
		RunnerID: runner.ID,
		JobID:    job.ID,
		RunID:    job.RunID,
		RepoID:   job.RepoID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner jwt mint failed", "runner_id", runner.ID, "job_id", job.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner token mint failed")
		return
	}
	writeJSON(w, http.StatusOK, presentRunnerClaim(job, steps, token, time.Unix(claims.Exp, 0)))
}

func (h *Handlers) authenticateRunner(w http.ResponseWriter, r *http.Request) (actionsdb.GetRunnerByTokenHashRow, bool) {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		writeAPIError(w, http.StatusUnauthorized, "runner token required")
		return actionsdb.GetRunnerByTokenHashRow{}, false
	}
	hash, err := runnertoken.HashOf(strings.TrimSpace(strings.TrimPrefix(authz, prefix)))
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "runner token invalid")
		return actionsdb.GetRunnerByTokenHashRow{}, false
	}
	runner, err := actionsdb.New().GetRunnerByTokenHash(r.Context(), h.d.Pool, hash)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "runner token invalid")
		return actionsdb.GetRunnerByTokenHashRow{}, false
	}
	return runner, true
}

func (h *Handlers) allowRunnerHeartbeat(w http.ResponseWriter, r *http.Request, runnerID int64) bool {
	if h.d.RateLimiter == nil {
		return true
	}
	decision, err := h.d.RateLimiter.Allow(r.Context(), runnerHeartbeatLimit, fmt.Sprintf("runner:%d", runnerID))
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "runner heartbeat rate-limit failed", "runner_id", runnerID, "error", err)
	}
	ratelimit.StampHeaders(w, decision)
	if !decision.Allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(decision.RetryAfter/time.Second)))
		writeAPIError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return false
	}
	return true
}

func (h *Handlers) claimRunnerJob(
	ctx context.Context,
	runnerID int64,
	labels []string,
	capacity int32,
) (actionsdb.ClaimQueuedWorkflowJobRow, []actionsdb.ListRunnerStepsForJobRow, bool, error) {
	q := actionsdb.New()
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := q.LockRunnerByID(ctx, tx, runnerID); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	running, err := q.CountRunningJobsForRunner(ctx, tx, runnerID)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	if running >= capacity {
		if _, err := q.HeartbeatRunner(ctx, tx, actionsdb.HeartbeatRunnerParams{
			ID:       runnerID,
			Labels:   labels,
			Capacity: capacity,
			Status:   actionsdb.WorkflowRunnerStatusBusy,
		}); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
		}
		committed = true
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, nil
	}

	job, err := q.ClaimQueuedWorkflowJob(ctx, tx, actionsdb.ClaimQueuedWorkflowJobParams{
		RunnerID: runnerID,
		Labels:   labels,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
		}
		if _, err := q.HeartbeatRunner(ctx, tx, actionsdb.HeartbeatRunnerParams{
			ID:       runnerID,
			Labels:   labels,
			Capacity: capacity,
			Status:   actionsdb.WorkflowRunnerStatusIdle,
		}); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
		}
		committed = true
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, nil
	}
	if err := q.MarkWorkflowRunRunning(ctx, tx, job.RunID); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	steps, err := q.ListRunnerStepsForJob(ctx, tx, job.ID)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	status := actionsdb.WorkflowRunnerStatusIdle
	if running+1 >= capacity {
		status = actionsdb.WorkflowRunnerStatusBusy
	}
	if _, err := q.HeartbeatRunner(ctx, tx, actionsdb.HeartbeatRunnerParams{
		ID:       runnerID,
		Labels:   labels,
		Capacity: capacity,
		Status:   status,
	}); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, false, err
	}
	committed = true
	return job, steps, true, nil
}

type runnerClaimResponse struct {
	Token     string           `json:"token"`
	ExpiresAt string           `json:"expires_at"`
	Job       runnerJobPayload `json:"job"`
}

type runnerJobPayload struct {
	ID             int64           `json:"id"`
	RunID          int64           `json:"run_id"`
	RepoID         int64           `json:"repo_id"`
	RunIndex       int64           `json:"run_index"`
	WorkflowFile   string          `json:"workflow_file"`
	WorkflowName   string          `json:"workflow_name"`
	HeadSHA        string          `json:"head_sha"`
	HeadRef        string          `json:"head_ref"`
	Event          string          `json:"event"`
	JobKey         string          `json:"job_key"`
	JobName        string          `json:"job_name"`
	RunsOn         string          `json:"runs_on"`
	Needs          []string        `json:"needs"`
	If             string          `json:"if"`
	TimeoutMinutes int32           `json:"timeout_minutes"`
	Permissions    json.RawMessage `json:"permissions"`
	Env            json.RawMessage `json:"env"`
	Steps          []runnerStep    `json:"steps"`
}

type runnerStep struct {
	ID               int64           `json:"id"`
	Index            int32           `json:"index"`
	StepID           string          `json:"step_id"`
	Name             string          `json:"name"`
	If               string          `json:"if"`
	Run              string          `json:"run"`
	Uses             string          `json:"uses"`
	WorkingDirectory string          `json:"working_directory"`
	Env              json.RawMessage `json:"env"`
	With             json.RawMessage `json:"with"`
	ContinueOnError  bool            `json:"continue_on_error"`
}

func presentRunnerClaim(
	job actionsdb.ClaimQueuedWorkflowJobRow,
	steps []actionsdb.ListRunnerStepsForJobRow,
	token string,
	expiresAt time.Time,
) runnerClaimResponse {
	outSteps := make([]runnerStep, 0, len(steps))
	for _, step := range steps {
		outSteps = append(outSteps, runnerStep{
			ID:               step.ID,
			Index:            step.StepIndex,
			StepID:           step.StepID,
			Name:             step.StepName,
			If:               step.IfExpr,
			Run:              step.RunCommand,
			Uses:             step.UsesAlias,
			WorkingDirectory: step.WorkingDirectory,
			Env:              rawJSONOrObject(step.StepEnv),
			With:             rawJSONOrObject(step.StepWith),
			ContinueOnError:  step.ContinueOnError,
		})
	}
	return runnerClaimResponse{
		Token:     token,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Job: runnerJobPayload{
			ID:             job.ID,
			RunID:          job.RunID,
			RepoID:         job.RepoID,
			RunIndex:       job.RunIndex,
			WorkflowFile:   job.WorkflowFile,
			WorkflowName:   job.WorkflowName,
			HeadSHA:        job.HeadSha,
			HeadRef:        job.HeadRef,
			Event:          string(job.Event),
			JobKey:         job.JobKey,
			JobName:        job.JobName,
			RunsOn:         job.RunsOn,
			Needs:          job.NeedsJobs,
			If:             job.IfExpr,
			TimeoutMinutes: job.TimeoutMinutes,
			Permissions:    rawJSONOrObject(job.Permissions),
			Env:            rawJSONOrObject(job.JobEnv),
			Steps:          outSteps,
		},
	}
}

func rawJSONOrObject(b []byte) json.RawMessage {
	if len(b) == 0 || !json.Valid(b) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}
