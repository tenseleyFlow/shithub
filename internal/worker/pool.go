// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// PoolConfig configures Pool. Leave fields zero for the documented
// defaults.
type PoolConfig struct {
	Workers    int           // default 4
	IdlePoll   time.Duration // default 5s — backstop when LISTEN drops a wake
	JobTimeout time.Duration // default 5min, applied per-job via context
	InstanceID string        // default "<hostname>:<pid>"
	Logger     *slog.Logger  // default discards
}

// Pool dispatches jobs from the queue. Construct via NewPool, register
// handlers via Register, run via Run.
type Pool struct {
	cfg      PoolConfig
	db       *pgxpool.Pool
	q        *workerdb.Queries
	handlers map[Kind]Handler
	rng      *mrand.Rand
	mu       sync.Mutex // guards handlers + rng
}

// NewPool wires a pool against an open pgx pool. Callers register
// handlers before calling Run.
func NewPool(db *pgxpool.Pool, cfg PoolConfig) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.IdlePoll <= 0 {
		cfg.IdlePoll = 5 * time.Second
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 5 * time.Minute
	}
	if cfg.InstanceID == "" {
		host, _ := os.Hostname()
		cfg.InstanceID = host + ":" + strconv.Itoa(os.Getpid())
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return &Pool{
		cfg:      cfg,
		db:       db,
		q:        workerdb.New(),
		handlers: make(map[Kind]Handler),
		// nolint:gosec // G404: jitter is non-cryptographic by design.
		rng: mrand.New(mrand.NewSource(time.Now().UnixNano())),
	}
}

// Register associates a handler with a kind. Re-registering replaces
// the previous handler. Registration is goroutine-safe so test harnesses
// can swap handlers between runs.
func (p *Pool) Register(kind Kind, h Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[kind] = h
}

// Run blocks until ctx is cancelled. Spawns cfg.Workers worker goroutines
// plus one LISTEN goroutine that fans out wake-ups. Returns nil after a
// clean drain; returns ctx.Err() if drain timed out.
func (p *Pool) Run(ctx context.Context) error {
	p.cfg.Logger.InfoContext(ctx, "worker: starting",
		"workers", p.cfg.Workers,
		"instance_id", p.cfg.InstanceID,
		"kinds", p.kindList())

	wake := make(chan struct{}, p.cfg.Workers)
	var wg sync.WaitGroup

	// LISTEN goroutine. Holds a dedicated conn for the lifetime of Run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.listenLoop(ctx, wake)
	}()

	for i := 0; i < p.cfg.Workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.workerLoop(ctx, id, wake)
		}(i)
	}

	wg.Wait()
	p.cfg.Logger.InfoContext(ctx, "worker: stopped")
	return nil
}

func (p *Pool) kindList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.handlers))
	for k := range p.handlers {
		out = append(out, string(k))
	}
	return out
}

// listenLoop maintains a LISTEN on NotifyChannel. On each NOTIFY, fan
// out to wake. On reconnect-required errors, sleeps briefly and retries.
func (p *Pool) listenLoop(ctx context.Context, wake chan<- struct{}) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := p.listenOnce(ctx, wake); err != nil && !errors.Is(err, context.Canceled) {
			p.cfg.Logger.WarnContext(ctx, "worker: listen restart", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (p *Pool) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := p.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	for {
		_, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		// Fan out to as many workers as are idle. Non-blocking sends so
		// we don't stall on a saturated pool.
		for i := 0; i < p.cfg.Workers; i++ {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}

func (p *Pool) workerLoop(ctx context.Context, id int, wake <-chan struct{}) {
	logger := p.cfg.Logger.With("worker_id", id)
	ticker := time.NewTicker(p.cfg.IdlePoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
		// Drain: try every registered kind; if any kind returned a job
		// loop back immediately without waiting on wake/tick.
		for {
			any, err := p.tryClaimAndRun(ctx, logger)
			if err != nil {
				logger.WarnContext(ctx, "worker: claim cycle error", "error", err)
				break
			}
			if !any {
				break
			}
		}
	}
}

// tryClaimAndRun walks every registered kind and attempts to claim one
// job. Returns true if any kind produced work this pass.
func (p *Pool) tryClaimAndRun(ctx context.Context, logger *slog.Logger) (bool, error) {
	p.mu.Lock()
	kinds := make([]Kind, 0, len(p.handlers))
	for k := range p.handlers {
		kinds = append(kinds, k)
	}
	p.mu.Unlock()

	any := false
	for _, kind := range kinds {
		if ctx.Err() != nil {
			return any, ctx.Err()
		}
		ran, err := p.runOne(ctx, kind, logger)
		if err != nil {
			return any, err
		}
		if ran {
			any = true
		}
	}
	return any, nil
}

// runOne claims one job of the given kind, runs it, and records the
// outcome. Returns ran=true when a job was claimed (regardless of
// success), false when the queue had nothing for this kind.
func (p *Pool) runOne(ctx context.Context, kind Kind, logger *slog.Logger) (bool, error) {
	job, err := p.q.ClaimJob(ctx, p.db, workerdb.ClaimJobParams{
		Kind:     string(kind),
		LockedBy: pgtype.Text{String: p.cfg.InstanceID, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", kind, err)
	}

	p.mu.Lock()
	h, ok := p.handlers[Kind(job.Kind)]
	p.mu.Unlock()
	if !ok {
		// Registered handler vanished between claim and dispatch; fail
		// the job rather than block the queue. Should never happen in
		// practice — Register is one-shot at boot.
		_ = p.q.MarkJobFailed(ctx, p.db, workerdb.MarkJobFailedParams{
			ID:        job.ID,
			LastError: pgtype.Text{String: "no handler registered", Valid: true},
		})
		return true, nil
	}

	jobCtx, cancel := context.WithTimeout(ctx, p.cfg.JobTimeout)
	start := time.Now()
	metrics.WorkerInFlight.WithLabelValues(job.Kind).Inc()
	runErr := safeRun(jobCtx, h, job.Payload)
	metrics.WorkerInFlight.WithLabelValues(job.Kind).Dec()
	cancel()
	metrics.WorkerJobDurationSeconds.WithLabelValues(job.Kind).Observe(time.Since(start).Seconds())

	logger.InfoContext(
		ctx, "worker: dispatched",
		"job_id", job.ID,
		"kind", job.Kind,
		"attempt", job.Attempts,
		"duration_ms", time.Since(start).Milliseconds(),
		"ok", runErr == nil,
	)

	if runErr == nil {
		if err := p.q.MarkJobCompleted(ctx, p.db, job.ID); err != nil {
			logger.ErrorContext(ctx, "worker: mark completed", "job_id", job.ID, "error", err)
		}
		metrics.WorkerJobsProcessedTotal.WithLabelValues(job.Kind, "ok").Inc()
		return true, nil
	}

	// Failure path. Poison errors skip retry.
	if errors.Is(runErr, ErrPoison) {
		_ = p.q.MarkJobFailed(ctx, p.db, workerdb.MarkJobFailedParams{
			ID:        job.ID,
			LastError: pgtype.Text{String: runErr.Error(), Valid: true},
		})
		metrics.WorkerJobsProcessedTotal.WithLabelValues(job.Kind, "poison").Inc()
		return true, nil
	}

	if int(job.Attempts) >= int(job.MaxAttempts) {
		_ = p.q.MarkJobFailed(ctx, p.db, workerdb.MarkJobFailedParams{
			ID:        job.ID,
			LastError: pgtype.Text{String: runErr.Error(), Valid: true},
		})
		metrics.WorkerJobsProcessedTotal.WithLabelValues(job.Kind, "failed").Inc()
		return true, nil
	}

	p.mu.Lock()
	delay := Backoff(int(job.Attempts), p.rng.Float64)
	p.mu.Unlock()
	_ = p.q.RescheduleJob(ctx, p.db, workerdb.RescheduleJobParams{
		ID:        job.ID,
		LastError: pgtype.Text{String: runErr.Error(), Valid: true},
		RunAt:     pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
	})
	metrics.WorkerJobsProcessedTotal.WithLabelValues(job.Kind, "retry").Inc()
	return true, nil
}

// safeRun wraps the handler in a recover so a panicking handler doesn't
// take the worker goroutine with it; the job is rescheduled like any
// other failure.
func safeRun(ctx context.Context, h Handler, payload json.RawMessage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("worker: handler panic: %v", r)
		}
	}()
	return h(ctx, payload)
}
