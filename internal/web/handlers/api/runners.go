// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsannotations "github.com/tenseleyFlow/shithub/internal/actions/annotations"
	actionsevents "github.com/tenseleyFlow/shithub/internal/actions/events"
	"github.com/tenseleyFlow/shithub/internal/actions/finalize"
	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	"github.com/tenseleyFlow/shithub/internal/actions/logstream"
	"github.com/tenseleyFlow/shithub/internal/actions/runnerlabels"
	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	actionstelemetry "github.com/tenseleyFlow/shithub/internal/actions/telemetry"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/runner/scrub"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

var runnerHeartbeatLimit = ratelimit.Policy{
	Scope:  "actions:runner_dispatch",
	Max:    60,
	Window: time.Minute,
}

func (h *Handlers) mountRunners(r chi.Router) {
	r.Post("/api/v1/runners/heartbeat", h.runnerHeartbeat)
	r.Post("/api/v1/jobs/{id}/logs", h.runnerJobLogs)
	r.Post("/api/v1/jobs/{id}/steps/{step_id}/status", h.runnerStepStatus)
	r.Post("/api/v1/jobs/{id}/status", h.runnerJobStatus)
	r.Post("/api/v1/jobs/{id}/artifacts/upload", h.runnerJobArtifactUpload)
	r.Post("/api/v1/jobs/{id}/cancel-check", h.runnerJobCancelCheck)
}

type runnerHeartbeatRequest struct {
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
	HostName string   `json:"host_name"`
	Version  string   `json:"version"`
}

const runnerMetadataMaxBytes = 255

var errRunnerRevoked = errors.New("runner is revoked")

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
	hostName := cleanRunnerMetadata(body.HostName)
	if hostName == "" {
		hostName = runner.HostName
	}
	version := cleanRunnerMetadata(body.Version)
	if version == "" {
		version = runner.Version
	}

	job, steps, resolvedSecrets, claimed, err := h.claimRunnerJob(r.Context(), runner.ID, labels, int32(capacity), hostName, version)
	if err != nil {
		if errors.Is(err, errRunnerRevoked) {
			metrics.ActionsRunnerHeartbeatsTotal.WithLabelValues("rejected").Inc()
			writeAPIError(w, http.StatusUnauthorized, "runner revoked")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "runner heartbeat claim failed", "runner_id", runner.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner heartbeat failed")
		return
	}
	if !claimed {
		metrics.ActionsRunnerHeartbeatsTotal.WithLabelValues("no_job").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	token, claims, err := h.d.RunnerJWT.Mint(runnerjwt.MintParams{
		RunnerID: runner.ID,
		JobID:    job.ID,
		RunID:    job.RunID,
		RepoID:   job.RepoID,
		Purpose:  runnerjwt.PurposeAPI,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner jwt mint failed", "runner_id", runner.ID, "job_id", job.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner token mint failed")
		return
	}
	checkoutToken, _, err := h.d.RunnerJWT.Mint(runnerjwt.MintParams{
		RunnerID: runner.ID,
		JobID:    job.ID,
		RunID:    job.RunID,
		RepoID:   job.RepoID,
		Purpose:  runnerjwt.PurposeCheckout,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner checkout token mint failed", "runner_id", runner.ID, "job_id", job.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner checkout token mint failed")
		return
	}
	metrics.ActionsRunnerHeartbeatsTotal.WithLabelValues("claimed").Inc()
	metrics.ActionsRunnerJWTTotal.WithLabelValues("issued").Add(2)
	writeJSON(w, http.StatusOK, h.presentRunnerClaim(job, steps, resolvedSecrets, token, checkoutToken, time.Unix(claims.Exp, 0)))
}

func cleanRunnerMetadata(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= runnerMetadataMaxBytes {
		return value
	}
	var b strings.Builder
	for _, r := range value {
		runeLen := utf8.RuneLen(r)
		if runeLen < 0 || b.Len()+runeLen > runnerMetadataMaxBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
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
	hostName string,
	version string,
) (actionsdb.ClaimQueuedWorkflowJobRow, []actionsdb.ListRunnerStepsForJobRow, map[string]string, bool, error) {
	q := actionsdb.New()
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	lockedRunner, err := q.LockRunnerByID(ctx, tx, runnerID)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	if lockedRunner.RevokedAt.Valid {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, errRunnerRevoked
	}
	running, err := q.CountRunningJobsForRunner(ctx, tx, runnerID)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	heartbeat := func(status actionsdb.WorkflowRunnerStatus) error {
		_, err := q.HeartbeatRunner(ctx, tx, actionsdb.HeartbeatRunnerParams{
			ID:       runnerID,
			Labels:   labels,
			Capacity: capacity,
			Status:   status,
			HostName: hostName,
			Version:  version,
		})
		return err
	}
	if lockedRunner.DrainingAt.Valid {
		status := actionsdb.WorkflowRunnerStatusIdle
		if running > 0 {
			status = actionsdb.WorkflowRunnerStatusBusy
		}
		if err := heartbeat(status); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		committed = true
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, nil
	}
	if running >= capacity {
		if err := heartbeat(actionsdb.WorkflowRunnerStatusBusy); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		committed = true
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, nil
	}

	job, err := q.ClaimQueuedWorkflowJob(ctx, tx, actionsdb.ClaimQueuedWorkflowJobParams{
		RunnerID: runnerID,
		Labels:   labels,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		if err := heartbeat(actionsdb.WorkflowRunnerStatusIdle); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
		committed = true
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, nil
	}
	run, err := q.StartWorkflowRun(ctx, tx, job.RunID)
	switch {
	case err == nil:
		if err := actionsevents.EmitRunTx(ctx, tx, run, actionsevents.ActionRunning); err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		run, err = q.GetWorkflowRunByID(ctx, tx, job.RunID)
		if err != nil {
			return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
		}
	default:
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	if err := actionsevents.EmitJobTx(ctx, tx, run, claimRowWorkflowJob(job), actionsevents.ActionRunning); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	steps, err := q.ListRunnerStepsForJob(ctx, tx, job.ID)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	resolvedSecrets, err := h.resolveVisibleSecretsFromDB(ctx, tx, job.RepoID, job.Event)
	if err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	if err := h.storeJobSecretMaskSnapshot(ctx, tx, job.ID, secretMaskValues(resolvedSecrets)); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	status := actionsdb.WorkflowRunnerStatusIdle
	if running+1 >= capacity {
		status = actionsdb.WorkflowRunnerStatusBusy
	}
	if err := heartbeat(status); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	var claimLatencySeconds float64
	observeClaimLatency := false
	if job.CreatedAt.Valid {
		if latency := time.Since(job.CreatedAt.Time); latency >= 0 {
			claimLatencySeconds = latency.Seconds()
			observeClaimLatency = true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return actionsdb.ClaimQueuedWorkflowJobRow{}, nil, nil, false, err
	}
	committed = true
	if observeClaimLatency {
		metrics.ActionsJobClaimLatencySeconds.Observe(claimLatencySeconds)
	}
	return job, steps, resolvedSecrets, true, nil
}

type runnerJobAuth struct {
	Claims   runnerjwt.Claims
	RunnerID int64
	Job      actionsdb.WorkflowJob
}

func (h *Handlers) authenticateRunnerJob(w http.ResponseWriter, r *http.Request) (runnerJobAuth, bool) {
	if h.d.RunnerJWT == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runner API is not configured")
		return runnerJobAuth{}, false
	}
	pathJobID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || pathJobID <= 0 {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return runnerJobAuth{}, false
	}
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		writeAPIError(w, http.StatusUnauthorized, "job token required")
		return runnerJobAuth{}, false
	}
	claims, err := h.d.RunnerJWT.Verify(strings.TrimSpace(strings.TrimPrefix(authz, prefix)))
	if err != nil {
		metrics.ActionsRunnerJWTTotal.WithLabelValues("rejected").Inc()
		writeAPIError(w, http.StatusUnauthorized, "job token invalid")
		return runnerJobAuth{}, false
	}
	if claims.Purpose != "" && claims.Purpose != runnerjwt.PurposeAPI {
		metrics.ActionsRunnerJWTTotal.WithLabelValues("rejected").Inc()
		writeAPIError(w, http.StatusUnauthorized, "job token invalid")
		return runnerJobAuth{}, false
	}
	if claims.JobID != pathJobID {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return runnerJobAuth{}, false
	}
	runnerID, err := claims.RunnerID()
	if err != nil {
		metrics.ActionsRunnerJWTTotal.WithLabelValues("rejected").Inc()
		writeAPIError(w, http.StatusUnauthorized, "job token invalid")
		return runnerJobAuth{}, false
	}
	q := actionsdb.New()
	runner, err := q.GetRunnerByID(r.Context(), h.d.Pool, runnerID)
	if err != nil || runner.RevokedAt.Valid {
		metrics.ActionsRunnerJWTTotal.WithLabelValues("rejected").Inc()
		writeAPIError(w, http.StatusUnauthorized, "job token invalid")
		return runnerJobAuth{}, false
	}
	job, err := q.GetWorkflowJobByID(r.Context(), h.d.Pool, pathJobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "job not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "job lookup failed")
		}
		return runnerJobAuth{}, false
	}
	if job.RunID != claims.RunID || !job.RunnerID.Valid || job.RunnerID.Int64 != runnerID {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return runnerJobAuth{}, false
	}
	if err := runnerjwt.Consume(r.Context(), h.d.Pool, claims); err != nil {
		if errors.Is(err, runnerjwt.ErrReplay) {
			metrics.ActionsRunnerJWTTotal.WithLabelValues("replay").Inc()
			writeAPIError(w, http.StatusUnauthorized, "job token replayed")
		} else {
			metrics.ActionsRunnerJWTTotal.WithLabelValues("rejected").Inc()
			h.d.Logger.ErrorContext(r.Context(), "runner jwt consume failed", "job_id", pathJobID, "error", err)
			writeAPIError(w, http.StatusUnauthorized, "job token invalid")
		}
		return runnerJobAuth{}, false
	}
	return runnerJobAuth{Claims: claims, RunnerID: runnerID, Job: job}, true
}

type runnerLogRequest struct {
	Seq    int32  `json:"seq"`
	Chunk  string `json:"chunk"`
	StepID int64  `json:"step_id,omitempty"`
}

func (h *Handlers) runnerJobLogs(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authenticateRunnerJob(w, r)
	if !ok {
		return
	}
	var body runnerLogRequest
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Seq < 0 {
		writeAPIError(w, http.StatusBadRequest, "seq must be non-negative")
		return
	}
	chunk, err := decodeBase64(body.Chunk)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "chunk must be base64")
		return
	}
	if len(chunk) == 0 || len(chunk) > 512*1024 {
		writeAPIError(w, http.StatusBadRequest, "chunk must be between 1 and 524288 bytes")
		return
	}
	values, err := h.jobSecretMaskValues(r.Context(), auth.Job.ID, auth.Claims.RepoID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner log mask resolution failed", "repo_id", auth.Claims.RepoID, "job_id", auth.Claims.JobID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "log mask resolution failed")
		return
	}
	stepID, ok := h.resolveLogStep(w, r, auth.Job.ID, body.StepID)
	if !ok {
		return
	}
	if err := h.appendScrubbedLogChunk(r.Context(), auth.Job.RunID, auth.Job.ID, stepID, body.Seq, chunk, values); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "append log failed")
		return
	}
	h.writeNextTokenResponse(w, r, http.StatusAccepted, auth, map[string]any{"accepted": true})
}

func (h *Handlers) resolveLogStep(w http.ResponseWriter, r *http.Request, jobID, stepID int64) (int64, bool) {
	q := actionsdb.New()
	if stepID == 0 {
		step, err := q.GetFirstStepForJob(r.Context(), h.d.Pool, jobID)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, "step not found")
			return 0, false
		}
		return step.ID, true
	}
	step, err := q.GetWorkflowStepByID(r.Context(), h.d.Pool, stepID)
	if err != nil || step.JobID != jobID {
		writeAPIError(w, http.StatusNotFound, "step not found")
		return 0, false
	}
	return step.ID, true
}

type runnerStatusRequest struct {
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

func (h *Handlers) runnerJobStatus(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authenticateRunnerJob(w, r)
	if !ok {
		return
	}
	var body runnerStatusRequest
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	update, terminal, err := normalizeJobStatusUpdate(auth.Job, body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, runCompleted, runConclusion, err := h.applyJobStatus(r.Context(), auth.Job, update)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "status update failed")
		return
	}
	if err := h.updateCheckRunForJob(r.Context(), updated); err != nil {
		h.d.Logger.WarnContext(r.Context(), "runner check_run update failed", "job_id", updated.ID, "error", err)
	}

	bodyMap := map[string]any{
		"status":     string(updated.Status),
		"conclusion": nullableConclusion(updated.Conclusion),
	}
	if runCompleted {
		bodyMap["run_status"] = "completed"
		bodyMap["run_conclusion"] = string(runConclusion)
	}
	if terminal {
		writeJSON(w, http.StatusOK, bodyMap)
		return
	}
	h.writeNextTokenResponse(w, r, http.StatusOK, auth, bodyMap)
}

func (h *Handlers) runnerStepStatus(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authenticateRunnerJob(w, r)
	if !ok {
		return
	}
	stepID, err := strconv.ParseInt(chi.URLParam(r, "step_id"), 10, 64)
	if err != nil || stepID <= 0 {
		writeAPIError(w, http.StatusNotFound, "step not found")
		return
	}
	q := actionsdb.New()
	step, err := q.GetWorkflowStepByID(r.Context(), h.d.Pool, stepID)
	if err != nil || step.JobID != auth.Job.ID {
		writeAPIError(w, http.StatusNotFound, "step not found")
		return
	}
	var body runnerStatusRequest
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	update, terminal, err := normalizeStepStatusUpdate(step, body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.applyStepStatus(r.Context(), step, update, terminal)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "step status update failed")
		return
	}
	recordStepTimeout(step, updated)
	if terminal && stepLifecycleChanged(step, updated) {
		actionstelemetry.RecordStepTerminal(updated)
	}
	h.writeNextTokenResponse(w, r, http.StatusOK, auth, map[string]any{
		"status":     string(updated.Status),
		"conclusion": nullableConclusion(updated.Conclusion),
	})
}

type normalizedJobStatusUpdate struct {
	Status      actionsdb.WorkflowJobStatus
	Conclusion  actionsdb.NullCheckConclusion
	StartedAt   pgtype.Timestamptz
	CompletedAt pgtype.Timestamptz
}

func normalizeJobStatusUpdate(job actionsdb.WorkflowJob, body runnerStatusRequest) (normalizedJobStatusUpdate, bool, error) {
	now := time.Now().UTC()
	status := actionsdb.WorkflowJobStatus(strings.TrimSpace(body.Status))
	if status == "" {
		return normalizedJobStatusUpdate{}, false, errors.New("status is required")
	}
	if !validWorkflowJobTransition(job.Status, status) {
		return normalizedJobStatusUpdate{}, false, fmt.Errorf("invalid status transition %s -> %s", job.Status, status)
	}
	startedAt := job.StartedAt
	if body.StartedAt != "" {
		t, err := parseTimeOptional(body.StartedAt)
		if err != nil {
			return normalizedJobStatusUpdate{}, false, fmt.Errorf("started_at: %w", err)
		}
		startedAt = pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
	}
	if !startedAt.Valid && (status == actionsdb.WorkflowJobStatusRunning ||
		status == actionsdb.WorkflowJobStatusCompleted ||
		status == actionsdb.WorkflowJobStatusCancelled) {
		startedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	completedAt := job.CompletedAt
	terminal := status == actionsdb.WorkflowJobStatusCompleted || status == actionsdb.WorkflowJobStatusCancelled
	if body.CompletedAt != "" {
		t, err := parseTimeOptional(body.CompletedAt)
		if err != nil {
			return normalizedJobStatusUpdate{}, false, fmt.Errorf("completed_at: %w", err)
		}
		completedAt = pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
	}
	if terminal && !completedAt.Valid {
		completedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	conclusion := actionsdb.NullCheckConclusion{}
	if terminal {
		c := strings.TrimSpace(body.Conclusion)
		if c == "" && status == actionsdb.WorkflowJobStatusCancelled {
			c = "cancelled"
		}
		if !validRunnerConclusion(c) {
			return normalizedJobStatusUpdate{}, false, errors.New("invalid or missing conclusion")
		}
		conclusion = actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusion(c), Valid: true}
	} else if strings.TrimSpace(body.Conclusion) != "" {
		return normalizedJobStatusUpdate{}, false, errors.New("conclusion is only valid for terminal statuses")
	}
	return normalizedJobStatusUpdate{
		Status:      status,
		Conclusion:  conclusion,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	}, terminal, nil
}

func validWorkflowJobTransition(from, to actionsdb.WorkflowJobStatus) bool {
	switch to {
	case actionsdb.WorkflowJobStatusRunning:
		return from == actionsdb.WorkflowJobStatusQueued || from == actionsdb.WorkflowJobStatusRunning
	case actionsdb.WorkflowJobStatusCompleted:
		return from == actionsdb.WorkflowJobStatusQueued || from == actionsdb.WorkflowJobStatusRunning || from == actionsdb.WorkflowJobStatusCompleted
	case actionsdb.WorkflowJobStatusCancelled:
		return from == actionsdb.WorkflowJobStatusQueued || from == actionsdb.WorkflowJobStatusRunning || from == actionsdb.WorkflowJobStatusCancelled
	default:
		return false
	}
}

type normalizedStepStatusUpdate struct {
	Status      actionsdb.WorkflowStepStatus
	Conclusion  actionsdb.NullCheckConclusion
	StartedAt   pgtype.Timestamptz
	CompletedAt pgtype.Timestamptz
}

func normalizeStepStatusUpdate(step actionsdb.WorkflowStep, body runnerStatusRequest) (normalizedStepStatusUpdate, bool, error) {
	now := time.Now().UTC()
	status := actionsdb.WorkflowStepStatus(strings.TrimSpace(body.Status))
	if status == "" {
		return normalizedStepStatusUpdate{}, false, errors.New("status is required")
	}
	if !validWorkflowStepTransition(step.Status, status) {
		return normalizedStepStatusUpdate{}, false, fmt.Errorf("invalid status transition %s -> %s", step.Status, status)
	}
	startedAt := step.StartedAt
	if body.StartedAt != "" {
		t, err := parseTimeOptional(body.StartedAt)
		if err != nil {
			return normalizedStepStatusUpdate{}, false, fmt.Errorf("started_at: %w", err)
		}
		startedAt = pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
	}
	if !startedAt.Valid && (status == actionsdb.WorkflowStepStatusRunning ||
		status == actionsdb.WorkflowStepStatusCompleted ||
		status == actionsdb.WorkflowStepStatusCancelled) {
		startedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	completedAt := step.CompletedAt
	terminal := status == actionsdb.WorkflowStepStatusCompleted ||
		status == actionsdb.WorkflowStepStatusCancelled ||
		status == actionsdb.WorkflowStepStatusSkipped
	if body.CompletedAt != "" {
		t, err := parseTimeOptional(body.CompletedAt)
		if err != nil {
			return normalizedStepStatusUpdate{}, false, fmt.Errorf("completed_at: %w", err)
		}
		completedAt = pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
	}
	if terminal && !completedAt.Valid {
		completedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	conclusion := actionsdb.NullCheckConclusion{}
	if terminal {
		c := strings.TrimSpace(body.Conclusion)
		if c == "" && status == actionsdb.WorkflowStepStatusCancelled {
			c = "cancelled"
		}
		if c == "" && status == actionsdb.WorkflowStepStatusSkipped {
			c = "skipped"
		}
		if !validRunnerConclusion(c) {
			return normalizedStepStatusUpdate{}, false, errors.New("invalid or missing conclusion")
		}
		conclusion = actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusion(c), Valid: true}
	} else if strings.TrimSpace(body.Conclusion) != "" {
		return normalizedStepStatusUpdate{}, false, errors.New("conclusion is only valid for terminal statuses")
	}
	return normalizedStepStatusUpdate{
		Status:      status,
		Conclusion:  conclusion,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	}, terminal, nil
}

func validWorkflowStepTransition(from, to actionsdb.WorkflowStepStatus) bool {
	switch to {
	case actionsdb.WorkflowStepStatusRunning:
		return from == actionsdb.WorkflowStepStatusQueued || from == actionsdb.WorkflowStepStatusRunning
	case actionsdb.WorkflowStepStatusCompleted:
		return from == actionsdb.WorkflowStepStatusQueued || from == actionsdb.WorkflowStepStatusRunning || from == actionsdb.WorkflowStepStatusCompleted
	case actionsdb.WorkflowStepStatusCancelled:
		return from == actionsdb.WorkflowStepStatusQueued || from == actionsdb.WorkflowStepStatusRunning || from == actionsdb.WorkflowStepStatusCancelled
	case actionsdb.WorkflowStepStatusSkipped:
		return from == actionsdb.WorkflowStepStatusQueued || from == actionsdb.WorkflowStepStatusRunning || from == actionsdb.WorkflowStepStatusSkipped
	default:
		return false
	}
}

func recordStepTimeout(before, after actionsdb.WorkflowStep) {
	if !after.Conclusion.Valid || after.Conclusion.CheckConclusion != actionsdb.CheckConclusionTimedOut {
		return
	}
	if before.Conclusion.Valid && before.Conclusion.CheckConclusion == actionsdb.CheckConclusionTimedOut {
		return
	}
	metrics.ActionsStepTimeoutsTotal.Inc()
}

func (h *Handlers) applyStepStatus(
	ctx context.Context,
	step actionsdb.WorkflowStep,
	update normalizedStepStatusUpdate,
	terminal bool,
) (actionsdb.WorkflowStep, error) {
	q := actionsdb.New()
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return actionsdb.WorkflowStep{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	updated, err := q.UpdateWorkflowStepStatus(ctx, tx, actionsdb.UpdateWorkflowStepStatusParams{
		ID:          step.ID,
		Status:      update.Status,
		Conclusion:  update.Conclusion,
		StartedAt:   update.StartedAt,
		CompletedAt: update.CompletedAt,
	})
	if err != nil {
		return actionsdb.WorkflowStep{}, err
	}
	shouldNotify := false
	if terminal && h.d.ObjectStore != nil {
		if _, err := worker.Enqueue(ctx, tx, finalize.KindWorkflowFinalizeStep, finalize.Payload{StepID: step.ID}, worker.EnqueueOptions{}); err != nil {
			return actionsdb.WorkflowStep{}, err
		}
		shouldNotify = true
	}
	if terminal {
		if err := logstream.NotifyDone(ctx, tx, step.ID); err != nil {
			return actionsdb.WorkflowStep{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return actionsdb.WorkflowStep{}, err
	}
	committed = true
	if shouldNotify {
		if err := worker.Notify(ctx, h.d.Pool); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "runner step finalizer notify failed", "step_id", step.ID, "error", err)
		}
	}
	return updated, nil
}

func (h *Handlers) applyJobStatus(
	ctx context.Context,
	job actionsdb.WorkflowJob,
	update normalizedJobStatusUpdate,
) (actionsdb.WorkflowJob, bool, actionsdb.CheckConclusion, error) {
	q := actionsdb.New()
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return actionsdb.WorkflowJob{}, false, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	updated, err := q.UpdateWorkflowJobStatus(ctx, tx, actionsdb.UpdateWorkflowJobStatusParams{
		ID:          job.ID,
		Status:      update.Status,
		Conclusion:  update.Conclusion,
		StartedAt:   update.StartedAt,
		CompletedAt: update.CompletedAt,
	})
	if err != nil {
		return actionsdb.WorkflowJob{}, false, "", err
	}
	notifyWorker := false
	if updated.Status == actionsdb.WorkflowJobStatusCancelled {
		steps, err := q.CancelOpenWorkflowStepsForJob(ctx, tx, updated.ID)
		if err != nil {
			return actionsdb.WorkflowJob{}, false, "", err
		}
		for _, step := range steps {
			if err := logstream.NotifyDone(ctx, tx, step.ID); err != nil {
				return actionsdb.WorkflowJob{}, false, "", err
			}
			if h.d.ObjectStore != nil {
				if _, err := worker.Enqueue(ctx, tx, finalize.KindWorkflowFinalizeStep, finalize.Payload{StepID: step.ID}, worker.EnqueueOptions{}); err != nil {
					return actionsdb.WorkflowJob{}, false, "", err
				}
				notifyWorker = true
			}
		}
	}
	jobs, err := q.ListJobsForRun(ctx, tx, updated.RunID)
	if err != nil {
		return actionsdb.WorkflowJob{}, false, "", err
	}
	runConclusion, complete := deriveWorkflowRunConclusion(jobs)
	runAfter, err := q.GetWorkflowRunByID(ctx, tx, updated.RunID)
	if err != nil {
		return actionsdb.WorkflowJob{}, false, "", err
	}
	runBefore := runAfter
	runStarted := false
	runTerminalChanged := false
	if complete {
		runAfter, err = q.CompleteWorkflowRun(ctx, tx, actionsdb.CompleteWorkflowRunParams{
			ID:         updated.RunID,
			Conclusion: runConclusion,
		})
		if err != nil {
			return actionsdb.WorkflowJob{}, false, "", err
		}
		runTerminalChanged = workflowRunLifecycleChanged(runBefore, runAfter)
	} else {
		startedRun, err := q.StartWorkflowRun(ctx, tx, updated.RunID)
		if err == nil {
			runAfter = startedRun
			runStarted = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return actionsdb.WorkflowJob{}, false, "", err
		}
	}
	if jobLifecycleChanged(job, updated) {
		if err := actionsevents.EmitJobTx(ctx, tx, runAfter, updated, workflowJobEventAction(updated.Status)); err != nil {
			return actionsdb.WorkflowJob{}, false, "", err
		}
	}
	if runStarted {
		if err := actionsevents.EmitRunTx(ctx, tx, runAfter, actionsevents.ActionRunning); err != nil {
			return actionsdb.WorkflowJob{}, false, "", err
		}
	}
	if complete && runTerminalChanged {
		if err := actionsevents.EmitRunTx(ctx, tx, runAfter, workflowRunEventAction(runAfter.Status)); err != nil {
			return actionsdb.WorkflowJob{}, false, "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return actionsdb.WorkflowJob{}, false, "", err
	}
	committed = true
	if notifyWorker {
		if err := worker.Notify(ctx, h.d.Pool); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "runner cancelled-step finalizer notify failed", "job_id", updated.ID, "error", err)
		}
	}
	if runTerminalChanged {
		actionstelemetry.RecordRunTerminal(runAfter)
	}
	return updated, complete, runConclusion, nil
}

func claimRowWorkflowJob(row actionsdb.ClaimQueuedWorkflowJobRow) actionsdb.WorkflowJob {
	return actionsdb.WorkflowJob{
		ID:              row.ID,
		RunID:           row.RunID,
		JobIndex:        row.JobIndex,
		JobKey:          row.JobKey,
		JobName:         row.JobName,
		RunsOn:          row.RunsOn,
		RunnerID:        row.RunnerID,
		NeedsJobs:       row.NeedsJobs,
		IfExpr:          row.IfExpr,
		TimeoutMinutes:  row.TimeoutMinutes,
		Permissions:     row.Permissions,
		JobEnv:          row.JobEnv,
		Status:          row.Status,
		Conclusion:      row.Conclusion,
		CancelRequested: row.CancelRequested,
		StartedAt:       row.StartedAt,
		CompletedAt:     row.CompletedAt,
		Version:         row.Version,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func jobLifecycleChanged(before, after actionsdb.WorkflowJob) bool {
	if before.Status != after.Status {
		return true
	}
	if before.Conclusion.Valid != after.Conclusion.Valid {
		return true
	}
	return before.Conclusion.Valid && before.Conclusion.CheckConclusion != after.Conclusion.CheckConclusion
}

func stepLifecycleChanged(before, after actionsdb.WorkflowStep) bool {
	if before.Status != after.Status {
		return true
	}
	if before.Conclusion.Valid != after.Conclusion.Valid {
		return true
	}
	return before.Conclusion.Valid && before.Conclusion.CheckConclusion != after.Conclusion.CheckConclusion
}

func workflowRunLifecycleChanged(before, after actionsdb.WorkflowRun) bool {
	if before.Status != after.Status {
		return true
	}
	if before.Conclusion.Valid != after.Conclusion.Valid {
		return true
	}
	return before.Conclusion.Valid && before.Conclusion.CheckConclusion != after.Conclusion.CheckConclusion
}

func workflowJobEventAction(status actionsdb.WorkflowJobStatus) string {
	switch status {
	case actionsdb.WorkflowJobStatusCancelled:
		return actionsevents.ActionCancelled
	case actionsdb.WorkflowJobStatusCompleted, actionsdb.WorkflowJobStatusSkipped:
		return actionsevents.ActionCompleted
	case actionsdb.WorkflowJobStatusRunning:
		return actionsevents.ActionRunning
	default:
		return actionsevents.ActionQueued
	}
}

func workflowRunEventAction(status actionsdb.WorkflowRunStatus) string {
	if status == actionsdb.WorkflowRunStatusCancelled {
		return actionsevents.ActionCancelled
	}
	return actionsevents.ActionCompleted
}

func deriveWorkflowRunConclusion(jobs []actionsdb.ListJobsForRunRow) (actionsdb.CheckConclusion, bool) {
	if len(jobs) == 0 {
		return actionsdb.CheckConclusionFailure, true
	}
	worst := actionsdb.CheckConclusionSuccess
	for _, job := range jobs {
		switch job.Status {
		case actionsdb.WorkflowJobStatusCompleted, actionsdb.WorkflowJobStatusCancelled, actionsdb.WorkflowJobStatusSkipped:
		default:
			return "", false
		}
		if job.Status == actionsdb.WorkflowJobStatusCancelled {
			worst = actionsdb.CheckConclusionCancelled
			continue
		}
		if !job.Conclusion.Valid {
			return actionsdb.CheckConclusionFailure, true
		}
		c := job.Conclusion.CheckConclusion
		if c == actionsdb.CheckConclusionFailure ||
			c == actionsdb.CheckConclusionTimedOut ||
			c == actionsdb.CheckConclusionActionRequired {
			return c, true
		}
		if c == actionsdb.CheckConclusionCancelled {
			worst = actionsdb.CheckConclusionCancelled
		}
	}
	return worst, true
}

func (h *Handlers) updateCheckRunForJob(ctx context.Context, job actionsdb.WorkflowJob) error {
	return actionslifecycle.SyncCheckRunForJob(ctx, actionslifecycle.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, job)
}

type runnerArtifactUploadRequest struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

var artifactNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (h *Handlers) runnerJobArtifactUpload(w http.ResponseWriter, r *http.Request) {
	if h.d.ObjectStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "object storage is not configured")
		return
	}
	auth, ok := h.authenticateRunnerJob(w, r)
	if !ok {
		return
	}
	var body runnerArtifactUploadRequest
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if !validArtifactName(body.Name) {
		writeAPIError(w, http.StatusBadRequest, "invalid artifact name")
		return
	}
	if body.SizeBytes < 0 {
		writeAPIError(w, http.StatusBadRequest, "size_bytes must be non-negative")
		return
	}
	if !h.enforceArtifactStorageQuota(w, r, auth.Claims.RepoID, body.SizeBytes) {
		return
	}
	objectKey := fmt.Sprintf("actions/runs/%d/artifacts/%s", auth.Claims.RunID, body.Name)
	artifact, err := actionsdb.New().InsertArtifact(r.Context(), h.d.Pool, actionsdb.InsertArtifactParams{
		RunID:     auth.Claims.RunID,
		Name:      body.Name,
		ObjectKey: objectKey,
		ByteCount: body.SizeBytes,
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().UTC().Add(90 * 24 * time.Hour),
			Valid: true,
		},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact create failed")
		return
	}
	uploadURL, err := h.d.ObjectStore.SignedURL(r.Context(), objectKey, 15*time.Minute, http.MethodPut)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact upload url failed")
		return
	}
	h.writeNextTokenResponse(w, r, http.StatusCreated, auth, map[string]any{
		"artifact_id": artifact.ID,
		"upload_url":  uploadURL,
	})
}

func (h *Handlers) enforceArtifactStorageQuota(w http.ResponseWriter, r *http.Request, repoID, sizeBytes int64) bool {
	if sizeBytes <= 0 {
		return true
	}
	repo, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, repoID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner artifact quota repo lookup failed",
			"repo_id", repoID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "artifact storage quota check failed")
		return false
	}
	if !repo.OwnerOrgID.Valid {
		return true
	}

	now := time.Now().UTC()
	periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
	counters, err := orgbilling.RecalculateOrgUsageCounters(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, repo.OwnerOrgID.Int64, periodStart, periodEnd)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner artifact quota usage recalc failed",
			"repo_id", repoID, "org_id", repo.OwnerOrgID.Int64, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "artifact storage quota check failed")
		return false
	}
	check, err := entitlements.CheckOrgStorageQuota(
		r.Context(),
		entitlements.Deps{Pool: h.d.Pool},
		repo.OwnerOrgID.Int64,
		counters.RepoStorageBytes+counters.ObjectStorageBytes,
		sizeBytes,
	)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner artifact quota check failed",
			"repo_id", repoID, "org_id", repo.OwnerOrgID.Int64, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "artifact storage quota check failed")
		return false
	}
	if !check.Allowed {
		writeAPIError(w, check.HTTPStatus(), check.Message())
		return false
	}
	return true
}

func validArtifactName(name string) bool {
	return len(name) >= 1 &&
		len(name) <= 100 &&
		artifactNameRE.MatchString(name) &&
		!strings.HasPrefix(name, "..") &&
		!strings.Contains(name, "/")
}

func (h *Handlers) runnerJobCancelCheck(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authenticateRunnerJob(w, r)
	if !ok {
		return
	}
	h.writeNextTokenResponse(w, r, http.StatusOK, auth, map[string]any{
		"cancelled": auth.Job.CancelRequested,
	})
}

type secretResolutionDB interface {
	actionsdb.DBTX
	reposdb.DBTX
}

func (h *Handlers) resolveVisibleSecrets(ctx context.Context, repoID int64) (map[string]string, error) {
	return h.resolveVisibleSecretsFromDB(ctx, h.d.Pool, repoID, "")
}

func (h *Handlers) resolveVisibleSecretsFromDB(ctx context.Context, db secretResolutionDB, repoID int64, event actionsdb.WorkflowRunEvent) (map[string]string, error) {
	if h.d.SecretBox == nil {
		return nil, nil
	}
	if event == actionsdb.WorkflowRunEventPullRequest {
		return nil, nil
	}
	repo, err := reposdb.New().GetRepoByID(ctx, db, repoID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if repo.OwnerOrgID.Valid {
		if err := h.mergeOrgSecrets(ctx, db, repo.OwnerOrgID.Int64, out); err != nil {
			return nil, err
		}
	}
	// PRO-EXT01-12b: user-scoped secrets layer between org/user and repo.
	// For a user-owned repo, the repo's owner has personal secrets that
	// apply to every workflow they run. Gated on FeatureUserActionsSecrets
	// — when the gate denies AND the enforce flag is set, the user-layer
	// is empty (so a Free user's workflows can't see user-scope rows
	// even if the write path slipped during the soak window).
	if repo.OwnerUserID.Valid {
		allowed, _, derr := h.userActionsSecretsAllowedForRunner(ctx, repo.OwnerUserID.Int64)
		if derr != nil {
			return nil, derr
		}
		if allowed || !h.d.BillingEnforce.UserActionsSecrets {
			if err := h.mergeUserSecrets(ctx, db, repo.OwnerUserID.Int64, out); err != nil {
				return nil, err
			}
		}
	}
	if err := h.mergeRepoSecrets(ctx, db, repo.ID, out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// mergeUserSecrets layers user-scope secrets into out. Same shape as
// mergeRepoSecrets/mergeOrgSecrets — repo-scope rows (resolved after
// this call) shadow user-scope rows with the same name, matching the
// repo-shadows-org precedence already in place.
func (h *Handlers) mergeUserSecrets(ctx context.Context, db actionsdb.DBTX, userID int64, out map[string]string) error {
	q := actionsdb.New()
	items, err := q.ListUserSecrets(ctx, db, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return err
	}
	for _, item := range items {
		row, err := q.GetUserSecret(ctx, db, actionsdb.GetUserSecretParams{
			UserID: pgtype.Int8{Int64: userID, Valid: true},
			Name:   item.Name,
		})
		if err != nil {
			return err
		}
		plaintext, err := h.d.SecretBox.Open(row.Ciphertext, row.Nonce)
		if err != nil {
			return err
		}
		out[item.Name] = string(plaintext)
	}
	return nil
}

// userActionsSecretsAllowedForRunner checks the FeatureUserActionsSecrets
// entitlement for the repo owner. Mirrors the handler-side check.
//
// PRO-EXT_SR-02: emits `entitlements.report_only_deny` when the
// owner lacks the entitlement, regardless of whether enforce is on.
// During report-only this is the only telemetry signal SREs have for
// the soak-window evidence required before PRO-EXT01-17 flips the
// enforce flag. After flip it stays useful as "denial would have
// fired again" staff-review signal.
func (h *Handlers) userActionsSecretsAllowedForRunner(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureUserActionsSecrets)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed && h.d.Logger != nil {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", orgbilling.PrincipalForUser(userID).String(),
			"principal_kind", string(orgbilling.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureUserActionsSecrets),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only",
			"surface", "runner")
	}
	return decision.Allowed, decision, nil
}

func (h *Handlers) mergeRepoSecrets(ctx context.Context, db actionsdb.DBTX, repoID int64, out map[string]string) error {
	q := actionsdb.New()
	items, err := q.ListRepoSecrets(ctx, db, pgtype.Int8{Int64: repoID, Valid: true})
	if err != nil {
		return err
	}
	for _, item := range items {
		row, err := q.GetRepoSecret(ctx, db, actionsdb.GetRepoSecretParams{
			RepoID: pgtype.Int8{Int64: repoID, Valid: true},
			Name:   item.Name,
		})
		if err != nil {
			return err
		}
		plaintext, err := h.d.SecretBox.Open(row.Ciphertext, row.Nonce)
		if err != nil {
			return err
		}
		out[item.Name] = string(plaintext)
	}
	return nil
}

func (h *Handlers) mergeOrgSecrets(ctx context.Context, db actionsdb.DBTX, orgID int64, out map[string]string) error {
	q := actionsdb.New()
	items, err := q.ListOrgSecrets(ctx, db, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		return err
	}
	for _, item := range items {
		row, err := q.GetOrgSecret(ctx, db, actionsdb.GetOrgSecretParams{
			OrgID: pgtype.Int8{Int64: orgID, Valid: true},
			Name:  item.Name,
		})
		if err != nil {
			return err
		}
		plaintext, err := h.d.SecretBox.Open(row.Ciphertext, row.Nonce)
		if err != nil {
			return err
		}
		out[item.Name] = string(plaintext)
	}
	return nil
}

func (h *Handlers) logMaskValues(ctx context.Context, repoID int64) ([]string, error) {
	resolved, err := h.resolveVisibleSecrets(ctx, repoID)
	if err != nil {
		return nil, err
	}
	return secretMaskValues(resolved), nil
}

func (h *Handlers) storeJobSecretMaskSnapshot(ctx context.Context, db actionsdb.DBTX, jobID int64, values []string) error {
	if h.d.SecretBox == nil {
		return nil
	}
	if values == nil {
		values = []string{}
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return err
	}
	ciphertext, nonce, err := h.d.SecretBox.Seal(payload)
	if err != nil {
		return err
	}
	return actionsdb.New().UpsertWorkflowJobSecretMask(ctx, db, actionsdb.UpsertWorkflowJobSecretMaskParams{
		JobID:      jobID,
		Ciphertext: ciphertext,
		Nonce:      nonce,
	})
}

func (h *Handlers) jobSecretMaskValues(ctx context.Context, jobID, repoID int64) ([]string, error) {
	if h.d.SecretBox == nil {
		return nil, nil
	}
	row, err := actionsdb.New().GetWorkflowJobSecretMask(ctx, h.d.Pool, jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return h.logMaskValues(ctx, repoID)
		}
		return nil, err
	}
	plaintext, err := h.d.SecretBox.Open(row.Ciphertext, row.Nonce)
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal(plaintext, &values); err != nil {
		return nil, err
	}
	sort.Strings(values)
	return values, nil
}

func secretMaskValues(resolved map[string]string) []string {
	if len(resolved) == 0 {
		return nil
	}
	values := make([]string, 0, len(resolved))
	for _, value := range resolved {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (h *Handlers) appendScrubbedLogChunk(ctx context.Context, runID, jobID, stepID int64, seq int32, chunk []byte, values []string) error {
	q := actionsdb.New()
	acceptedChunkBytes := len(chunk)
	if len(values) == 0 {
		row, err := q.AppendStepLogChunk(ctx, h.d.Pool, actionsdb.AppendStepLogChunkParams{
			StepID: stepID,
			Seq:    seq,
			Chunk:  chunk,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := h.storeLogAnnotations(ctx, h.d.Pool, runID, jobID, stepID, seq, chunk, values); err != nil {
			return err
		}
		metrics.ActionsLogChunksTotal.WithLabelValues("server").Inc()
		metrics.ActionsLogChunkBytesTotal.WithLabelValues("server").Add(float64(acceptedChunkBytes))
		return logstream.NotifyChunk(ctx, h.d.Pool, stepID, row.Seq)
	}

	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := q.GetStepLogChunkByStepSeq(ctx, tx, actionsdb.GetStepLogChunkByStepSeqParams{
		StepID: stepID,
		Seq:    seq,
	}); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	var replacements uint64
	prev, err := q.GetStepLogChunkBefore(ctx, tx, actionsdb.GetStepLogChunkBeforeParams{
		StepID: stepID,
		Seq:    seq,
	})
	switch {
	case err == nil:
		if carry := scrubCarryLen(prev.Chunk, values); carry > 0 {
			prefix := append([]byte(nil), prev.Chunk[:len(prev.Chunk)-carry]...)
			combined := append(append([]byte(nil), prev.Chunk[len(prev.Chunk)-carry:]...), chunk...)
			chunk, replacements = scrubChunk(combined, values)
			if err := q.UpdateStepLogChunk(ctx, tx, actionsdb.UpdateStepLogChunkParams{
				ID:    prev.ID,
				Chunk: prefix,
			}); err != nil {
				return err
			}
		} else {
			chunk, replacements = scrubChunk(chunk, values)
		}
	case errors.Is(err, pgx.ErrNoRows):
		chunk, replacements = scrubChunk(chunk, values)
	default:
		return err
	}

	accepted := false
	row, err := q.AppendStepLogChunk(ctx, tx, actionsdb.AppendStepLogChunkParams{
		StepID: stepID,
		Seq:    seq,
		Chunk:  chunk,
	})
	switch {
	case err == nil:
		accepted = true
		if err := h.storeLogAnnotations(ctx, tx, runID, jobID, stepID, seq, chunk, values); err != nil {
			return err
		}
		if err := logstream.NotifyChunk(ctx, tx, stepID, row.Seq); err != nil {
			return err
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if accepted {
		if replacements > 0 {
			metrics.ActionsLogScrubReplacementsTotal.WithLabelValues("server").Add(float64(replacements))
		}
		metrics.ActionsLogChunksTotal.WithLabelValues("server").Inc()
		metrics.ActionsLogChunkBytesTotal.WithLabelValues("server").Add(float64(acceptedChunkBytes))
	}
	return nil
}

func (h *Handlers) storeLogAnnotations(ctx context.Context, db actionsdb.DBTX, runID, jobID, stepID int64, seq int32, chunk []byte, values []string) error {
	parsed := actionsannotations.ParseChunk(chunk, seq, values)
	if len(parsed) == 0 {
		return nil
	}
	q := actionsdb.New()
	for _, ann := range parsed {
		if _, err := q.UpsertWorkflowAnnotation(ctx, db, actionsdb.UpsertWorkflowAnnotationParams{
			RunID:       runID,
			JobID:       jobID,
			StepID:      stepID,
			Level:       workflowAnnotationLevel(ann.Level),
			Title:       ann.Title,
			Message:     ann.Message,
			Path:        ann.Path,
			StartLine:   annotationInt4(ann.StartLine),
			EndLine:     annotationInt4(ann.EndLine),
			StartColumn: annotationInt4(ann.StartColumn),
			EndColumn:   annotationInt4(ann.EndColumn),
			LogLine:     annotationInt4(ann.LogLine),
			LogChunkSeq: annotationInt4(ann.LogChunkSeq),
			Fingerprint: ann.Fingerprint,
		}); err != nil {
			return err
		}
	}
	return nil
}

func workflowAnnotationLevel(level string) actionsdb.WorkflowAnnotationLevel {
	switch level {
	case actionsannotations.LevelError:
		return actionsdb.WorkflowAnnotationLevelError
	case actionsannotations.LevelNotice:
		return actionsdb.WorkflowAnnotationLevelNotice
	default:
		return actionsdb.WorkflowAnnotationLevelWarning
	}
}

func annotationInt4(value int32) pgtype.Int4 {
	if value <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: value, Valid: true}
}

func scrubChunk(chunk []byte, values []string) ([]byte, uint64) {
	if len(values) == 0 {
		return chunk, 0
	}
	s := scrub.New(values)
	out := s.Scrub(chunk)
	return append(out, s.Flush()...), s.Replacements()
}

func scrubCarryLen(chunk []byte, values []string) int {
	if len(chunk) == 0 || len(values) == 0 {
		return 0
	}
	text := string(chunk)
	keep := 0
	for _, value := range values {
		if value == "" {
			continue
		}
		max := len(value) - 1
		if max > len(text) {
			max = len(text)
		}
		for n := max; n > keep; n-- {
			if strings.HasSuffix(text, value[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}

func (h *Handlers) writeNextTokenResponse(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	auth runnerJobAuth,
	body map[string]any,
) {
	token, claims, err := h.d.RunnerJWT.Mint(runnerjwt.MintParams{
		RunnerID: auth.RunnerID,
		JobID:    auth.Claims.JobID,
		RunID:    auth.Claims.RunID,
		RepoID:   auth.Claims.RepoID,
		Purpose:  runnerjwt.PurposeAPI,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "runner next-token mint failed", "job_id", auth.Claims.JobID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "runner token mint failed")
		return
	}
	metrics.ActionsRunnerJWTTotal.WithLabelValues("issued").Inc()
	body["next_token"] = token
	body["next_token_expires_at"] = time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)
	writeJSON(w, status, body)
}

type runnerClaimResponse struct {
	Token     string           `json:"token"`
	ExpiresAt string           `json:"expires_at"`
	Job       runnerJobPayload `json:"job"`
}

type runnerJobPayload struct {
	ID             int64             `json:"id"`
	RunID          int64             `json:"run_id"`
	RepoID         int64             `json:"repo_id"`
	RunIndex       int64             `json:"run_index"`
	WorkflowFile   string            `json:"workflow_file"`
	WorkflowName   string            `json:"workflow_name"`
	CheckoutURL    string            `json:"checkout_url"`
	CheckoutToken  string            `json:"checkout_token"`
	HeadSHA        string            `json:"head_sha"`
	HeadRef        string            `json:"head_ref"`
	Event          string            `json:"event"`
	EventPayload   json.RawMessage   `json:"event_payload"`
	JobKey         string            `json:"job_key"`
	JobName        string            `json:"job_name"`
	RunsOn         string            `json:"runs_on"`
	Needs          []string          `json:"needs"`
	If             string            `json:"if"`
	TimeoutMinutes int32             `json:"timeout_minutes"`
	Permissions    json.RawMessage   `json:"permissions"`
	Secrets        map[string]string `json:"secrets"`
	MaskValues     []string          `json:"mask_values"`
	Env            json.RawMessage   `json:"env"`
	Steps          []runnerStep      `json:"steps"`
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

func (h *Handlers) presentRunnerClaim(
	job actionsdb.ClaimQueuedWorkflowJobRow,
	steps []actionsdb.ListRunnerStepsForJobRow,
	resolvedSecrets map[string]string,
	token string,
	checkoutToken string,
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
			CheckoutURL:    h.checkoutURL(job.RepoOwner, job.RepoName),
			CheckoutToken:  checkoutToken,
			HeadSHA:        job.HeadSha,
			HeadRef:        job.HeadRef,
			Event:          string(job.Event),
			EventPayload:   rawJSONOrObject(job.EventPayload),
			JobKey:         job.JobKey,
			JobName:        job.JobName,
			RunsOn:         job.RunsOn,
			Needs:          job.NeedsJobs,
			If:             job.IfExpr,
			TimeoutMinutes: job.TimeoutMinutes,
			Permissions:    rawJSONOrObject(job.Permissions),
			Secrets:        cloneStringMap(resolvedSecrets),
			MaskValues:     secretMaskValues(resolvedSecrets),
			Env:            rawJSONOrObject(job.JobEnv),
			Steps:          outSteps,
		},
	}
}

func (h *Handlers) checkoutURL(owner, repoName string) string {
	base := strings.TrimRight(strings.TrimSpace(h.d.BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + ".git"
}

func rawJSONOrObject(b []byte) json.RawMessage {
	if len(b) == 0 || !json.Valid(b) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

func decodeJSONBody(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func decodeBase64(s string) ([]byte, error) {
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return out, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func validRunnerConclusion(c string) bool {
	switch actionsdb.CheckConclusion(c) {
	case actionsdb.CheckConclusionSuccess,
		actionsdb.CheckConclusionFailure,
		actionsdb.CheckConclusionNeutral,
		actionsdb.CheckConclusionCancelled,
		actionsdb.CheckConclusionSkipped,
		actionsdb.CheckConclusionTimedOut,
		actionsdb.CheckConclusionActionRequired:
		return true
	default:
		return false
	}
}

func nullableConclusion(c actionsdb.NullCheckConclusion) any {
	if !c.Valid {
		return nil
	}
	return string(c.CheckConclusion)
}
