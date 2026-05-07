// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// testKind is unique per test so handlers don't bleed across parallel
// tests sharing the worker package's runtime state.
const (
	testKindHappy   worker.Kind = "test:happy"
	testKindRetry   worker.Kind = "test:retry"
	testKindPoison  worker.Kind = "test:poison"
	testKindFanIn50 worker.Kind = "test:fanin50"
)

// runUntil starts the pool in a goroutine and returns a stop func that
// cancels the context and waits for clean exit.
func runPool(t *testing.T, p *worker.Pool) (cancel func()) {
	t.Helper()
	ctx, c := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = p.Run(ctx)
		close(done)
	}()
	return func() {
		c()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("pool did not stop in 10s")
		}
	}
}

func TestPool_HappyPath(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)

	var seen atomic.Int64
	p := worker.NewPool(pool, worker.PoolConfig{Workers: 2, IdlePoll: 200 * time.Millisecond})
	p.Register(testKindHappy, func(_ context.Context, _ json.RawMessage) error {
		seen.Add(1)
		return nil
	})
	stop := runPool(t, p)
	defer stop()

	id, err := worker.Enqueue(context.Background(), pool, testKindHappy, map[string]any{"x": 1}, worker.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := worker.Notify(context.Background(), pool); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return seen.Load() == 1 })

	q := workerdb.New()
	job, err := q.GetJob(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.CompletedAt.Valid {
		t.Errorf("job %d: completed_at unset; last_error=%v", id, job.LastError.String)
	}
}

func TestPool_RetryThenSucceed(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)

	var attempts atomic.Int64
	p := worker.NewPool(pool, worker.PoolConfig{Workers: 2, IdlePoll: 100 * time.Millisecond})
	p.Register(testKindRetry, func(_ context.Context, _ json.RawMessage) error {
		if attempts.Add(1) < 2 {
			return errors.New("transient")
		}
		return nil
	})
	stop := runPool(t, p)
	defer stop()

	// Enqueue with run_at well in the past so reschedule fires immediately.
	id, err := worker.Enqueue(context.Background(), pool, testKindRetry, map[string]any{}, worker.EnqueueOptions{MaxAttempts: 5})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_ = worker.Notify(context.Background(), pool)

	// Force the rescheduled run_at to "now" so we don't wait the full
	// backoff. We do this by polling: after the first attempt fails,
	// the row's run_at is base * 2 ≈ 60s. We bypass via direct UPDATE.
	q := workerdb.New()
	waitFor(t, 5*time.Second, func() bool { return attempts.Load() >= 1 })
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET run_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("force run_at: %v", err)
	}
	_ = worker.Notify(context.Background(), pool)

	waitFor(t, 5*time.Second, func() bool { return attempts.Load() >= 2 })
	waitFor(t, 5*time.Second, func() bool {
		j, err := q.GetJob(context.Background(), pool, id)
		return err == nil && j.CompletedAt.Valid
	})
}

func TestPool_PoisonGoesStraightToFailed(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)

	var calls atomic.Int64
	p := worker.NewPool(pool, worker.PoolConfig{Workers: 1, IdlePoll: 100 * time.Millisecond})
	p.Register(testKindPoison, func(_ context.Context, _ json.RawMessage) error {
		calls.Add(1)
		return worker.PoisonError(errors.New("nope"))
	})
	stop := runPool(t, p)
	defer stop()

	id, err := worker.Enqueue(context.Background(), pool, testKindPoison, map[string]any{}, worker.EnqueueOptions{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_ = worker.Notify(context.Background(), pool)

	q := workerdb.New()
	waitFor(t, 5*time.Second, func() bool {
		j, err := q.GetJob(context.Background(), pool, id)
		return err == nil && j.FailedAt.Valid
	})
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on poison)", got)
	}
	j, _ := q.GetJob(context.Background(), pool, id)
	if !j.LastError.Valid || j.LastError.String == "" {
		t.Errorf("last_error not recorded on poison")
	}
}

func TestPool_ConcurrentClaimsExactlyOnce(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)

	const total = 50
	processed := make(map[int64]int) // job_id → times processed
	var mu sync.Mutex
	p := worker.NewPool(pool, worker.PoolConfig{Workers: 4, IdlePoll: 50 * time.Millisecond})
	p.Register(testKindFanIn50, func(_ context.Context, raw json.RawMessage) error {
		var payload struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		processed[payload.ID]++
		mu.Unlock()
		return nil
	})
	stop := runPool(t, p)
	defer stop()

	for i := 0; i < total; i++ {
		_, err := worker.Enqueue(context.Background(), pool, testKindFanIn50,
			map[string]any{"id": i}, worker.EnqueueOptions{})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	_ = worker.Notify(context.Background(), pool)

	waitFor(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) == total
	})

	mu.Lock()
	defer mu.Unlock()
	for id, count := range processed {
		if count != 1 {
			t.Errorf("job %d processed %d times, want 1", id, count)
		}
	}
}

func TestEnqueue_DelayedRunAt(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	future := time.Now().Add(1 * time.Hour)
	id, err := worker.Enqueue(context.Background(), pool, "test:delayed", map[string]any{}, worker.EnqueueOptions{
		RunAt: pgtype.Timestamptz{Time: future, Valid: true},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	q := workerdb.New()
	job, err := q.GetJob(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.RunAt.Time.Equal(future.UTC().Truncate(time.Microsecond)) {
		// pg truncates to microseconds; allow a tiny delta.
		if d := job.RunAt.Time.Sub(future); d > time.Second || d < -time.Second {
			t.Errorf("run_at = %v, want %v", job.RunAt.Time, future)
		}
	}
}

// waitFor polls cond every 50ms up to limit. Fails the test on timeout.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %v", limit)
}
