// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/logstream"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	actionsLogStreamBatchSize      = int32(100)
	actionsLogStreamHeartbeatEvery = 20 * time.Second
	actionsLogStreamReleaseTimeout = 3 * time.Second
)

var actionsLogStreamLimit = ratelimit.Policy{
	Scope:  "actions:logtail",
	Max:    5,
	Window: 2 * time.Minute,
}

type actionsLogStreamChunk struct {
	Seq      int32  `json:"seq"`
	ChunkB64 string `json:"chunk_b64"`
}

func (h *Handlers) repoActionStepLogStream(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	runIndex, ok := parsePositiveInt64Param(r, "runIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	jobIndex, ok := parseNonNegativeInt32Param(r, "jobIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	stepIndex, ok := parseNonNegativeInt32Param(r, "stepIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	lastSeq, ok := parseLogStreamAfter(r)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid Last-Event-ID")
		return
	}

	run, err := h.loadActionsRunDetail(r.Context(), row.ID, owner.Username, row.Name, runIndex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: get run for step log stream", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	_, step, ok := findActionStep(run, jobIndex, stepIndex)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	lease, decision, leaseErr := h.acquireLogStreamLease(r.Context(), w, r)
	if leaseErr != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: log stream rate-limit failed", "step_id", step.ID, "error", leaseErr)
	}
	if !decision.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter/time.Second)))
		h.d.Render.HTTPError(w, r, http.StatusTooManyRequests, "too many live log streams")
		return
	}
	if lease != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), actionsLogStreamReleaseTimeout)
			defer cancel()
			if err := lease.Release(ctx); err != nil && h.d.Logger != nil {
				h.d.Logger.WarnContext(r.Context(), "repo actions: release log stream lease", "step_id", step.ID, "error", err)
			}
		}()
	}

	conn, err := h.d.Pool.Acquire(r.Context())
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: acquire log stream conn", "step_id", step.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer conn.Release()
	if _, err := conn.Exec(r.Context(), logstream.ListenSQL(step.ID)); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: listen log stream", "step_id", step.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	nextSeq, err := h.flushStepLogChunks(r.Context(), w, conn, step.ID, lastSeq)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: write initial log chunks", "step_id", step.ID, "error", err)
		return
	}
	done, err := h.stepLogStreamDone(r.Context(), conn, step.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: check step terminal", "step_id", step.ID, "error", err)
		return
	}
	if done {
		_ = writeSSEEvent(w, "done", -1, []byte(`{}`))
		flusher.Flush()
		return
	}
	flusher.Flush()

	for {
		waitCtx, cancel := context.WithTimeout(r.Context(), actionsLogStreamHeartbeatEvery)
		notification, err := conn.Conn().WaitForNotification(waitCtx)
		cancel()
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			nextSeq, err = h.flushStepLogChunks(r.Context(), w, conn, step.ID, nextSeq)
			if err != nil {
				h.d.Logger.WarnContext(r.Context(), "repo actions: write heartbeat log chunks", "step_id", step.ID, "error", err)
				return
			}
			done, err := h.stepLogStreamDone(r.Context(), conn, step.ID)
			if err != nil {
				h.d.Logger.WarnContext(r.Context(), "repo actions: heartbeat terminal check", "step_id", step.ID, "error", err)
				return
			}
			if done {
				_ = writeSSEEvent(w, "done", -1, []byte(`{}`))
				flusher.Flush()
				return
			}
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "repo actions: wait log notification", "step_id", step.ID, "error", err)
			return
		}
		if notification.Channel != logstream.Channel(step.ID) {
			continue
		}
		_, done, ok := logstream.ParsePayload(notification.Payload)
		if !ok {
			h.d.Logger.WarnContext(r.Context(), "repo actions: invalid log notification", "step_id", step.ID, "payload", notification.Payload)
			continue
		}
		nextSeq, err = h.flushStepLogChunks(r.Context(), w, conn, step.ID, nextSeq)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "repo actions: write log chunks", "step_id", step.ID, "error", err)
			return
		}
		if done {
			_ = writeSSEEvent(w, "done", -1, []byte(`{}`))
			flusher.Flush()
			return
		}
		flusher.Flush()
	}
}

func (h *Handlers) acquireLogStreamLease(ctx context.Context, w http.ResponseWriter, r *http.Request) (*ratelimit.Lease, ratelimit.Decision, error) {
	if h.d.RateLimiter == nil {
		return nil, ratelimit.Decision{Allowed: true}, nil
	}
	key := logStreamRateLimitKey(r)
	if key == "" {
		return nil, ratelimit.Decision{Allowed: true}, nil
	}
	lease, decision, err := h.d.RateLimiter.AcquireLease(ctx, actionsLogStreamLimit, key)
	ratelimit.StampHeaders(w, decision)
	return lease, decision, err
}

func logStreamRateLimitKey(r *http.Request) string {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !viewer.IsAnonymous() {
		return "u:" + strconv.FormatInt(viewer.ID, 10)
	}
	if ip, ok := ratelimit.ClientIP(r, true); ok {
		return "ip:" + ip.String()
	}
	return ""
}

func parseLogStreamAfter(r *http.Request) (int32, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return -1, true
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < -1 {
		return 0, false
	}
	return int32(n), true
}

func (h *Handlers) flushStepLogChunks(ctx context.Context, w http.ResponseWriter, db actionsdb.DBTX, stepID int64, afterSeq int32) (int32, error) {
	q := actionsdb.New()
	nextSeq := afterSeq
	for {
		chunks, err := q.ListStepLogChunks(ctx, db, actionsdb.ListStepLogChunksParams{
			StepID: stepID,
			Seq:    nextSeq,
			Limit:  actionsLogStreamBatchSize,
		})
		if err != nil {
			return nextSeq, err
		}
		for _, chunk := range chunks {
			payload, err := json.Marshal(actionsLogStreamChunk{
				Seq:      chunk.Seq,
				ChunkB64: base64.StdEncoding.EncodeToString(chunk.Chunk),
			})
			if err != nil {
				return nextSeq, err
			}
			if err := writeSSEEvent(w, "chunk", chunk.Seq, payload); err != nil {
				return nextSeq, err
			}
			nextSeq = chunk.Seq
		}
		if int32(len(chunks)) < actionsLogStreamBatchSize {
			return nextSeq, nil
		}
	}
}

func (h *Handlers) stepLogStreamDone(ctx context.Context, db actionsdb.DBTX, stepID int64) (bool, error) {
	step, err := actionsdb.New().GetWorkflowStepByID(ctx, db, stepID)
	if err != nil {
		return false, err
	}
	return workflowStepTerminal(step.Status), nil
}

func writeSSEEvent(w http.ResponseWriter, event string, id int32, payload []byte) error {
	if id >= 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", id); err != nil {
			return err
		}
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}
