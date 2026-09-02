// SPDX-License-Identifier: AGPL-3.0-or-later

package traffic

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPurgeBatchedStopsOnShortBatch(t *testing.T) {
	// 12 rows at a batch size of 5: two full batches, then a short one.
	remaining := int64(12)
	var calls []int64
	total, more, err := purgeBatched(context.Background(), 100, 5,
		func(_ context.Context, limit int64) (int64, error) {
			calls = append(calls, limit)
			n := min(limit, remaining)
			remaining -= n
			return n, nil
		})
	if err != nil {
		t.Fatalf("purgeBatched: %v", err)
	}
	if total != 12 {
		t.Fatalf("total = %d, want 12", total)
	}
	if more {
		t.Fatal("more = true, want false: the loop drained the table")
	}
	if len(calls) != 3 {
		t.Fatalf("statements = %d (%v), want 3", len(calls), calls)
	}
	for i, limit := range calls {
		if limit != 5 {
			t.Fatalf("statement %d ran with limit %d, want the batch size 5", i, limit)
		}
	}
}

// An exact multiple of the batch size costs one extra empty DELETE; the
// loop must still terminate rather than spin to MaxBatches.
func TestPurgeBatchedStopsOnExactMultiple(t *testing.T) {
	remaining := int64(10)
	calls := 0
	total, more, err := purgeBatched(context.Background(), 100, 5,
		func(_ context.Context, limit int64) (int64, error) {
			calls++
			n := min(limit, remaining)
			remaining -= n
			return n, nil
		})
	if err != nil {
		t.Fatalf("purgeBatched: %v", err)
	}
	if total != 10 || more {
		t.Fatalf("total = %d, more = %v, want 10 and false", total, more)
	}
	if calls != 3 {
		t.Fatalf("statements = %d, want 3 (two full batches plus the empty one)", calls)
	}
}

func TestPurgeBatchedHonoursMaxBatches(t *testing.T) {
	calls := 0
	total, more, err := purgeBatched(context.Background(), 4, 5,
		func(_ context.Context, limit int64) (int64, error) {
			calls++
			return limit, nil // always a full batch: backlog never drains
		})
	if err != nil {
		t.Fatalf("purgeBatched: %v", err)
	}
	if calls != 4 {
		t.Fatalf("statements = %d, want the 4 the cap allows", calls)
	}
	if total != 20 {
		t.Fatalf("total = %d, want 20", total)
	}
	if !more {
		t.Fatal("more = false, want true: the run stopped on the batch cap")
	}
}

func TestPurgeBatchedZeroBounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxBatches int
		batch      int64
	}{
		{"zero batch size", 100, 0},
		{"negative batch size", 100, -5},
		{"zero max batches", 0, 5},
		{"negative max batches", -1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total, more, err := purgeBatched(context.Background(), tc.maxBatches, tc.batch,
				func(context.Context, int64) (int64, error) {
					t.Fatal("delete ran with a non-positive bound")
					return 0, nil
				})
			if err != nil || total != 0 || more {
				t.Fatalf("total = %d, more = %v, err = %v; want 0, false, nil", total, more, err)
			}
		})
	}
}

func TestPurgeBatchedReturnsPartialProgressOnError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	total, more, err := purgeBatched(context.Background(), 100, 5,
		func(_ context.Context, limit int64) (int64, error) {
			calls++
			if calls == 2 {
				return 2, boom
			}
			return limit, nil
		})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	// Rows the failing statement did delete still count: each DELETE is
	// its own transaction, so a mid-loop failure does not roll them back.
	if total != 7 {
		t.Fatalf("total = %d, want 7 (5 from the first batch, 2 from the failed one)", total)
	}
	if !more {
		t.Fatal("more = false, want true: the table was not drained")
	}
}

func TestPurgeBatchedStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	total, more, err := purgeBatched(ctx, 100, 5,
		func(context.Context, int64) (int64, error) {
			t.Fatal("delete ran after the context was canceled")
			return 0, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if total != 0 || !more {
		t.Fatalf("total = %d, more = %v, want 0 and true", total, more)
	}
}

func TestPurgeOptionsNormalize(t *testing.T) {
	got := PurgeOptions{}.normalize()
	if got.RetentionDays != DefaultRetentionDays {
		t.Fatalf("RetentionDays = %d, want %d", got.RetentionDays, DefaultRetentionDays)
	}
	if got.DailyRetentionDays != DefaultDailyRetentionDays {
		t.Fatalf("DailyRetentionDays = %d, want %d", got.DailyRetentionDays, DefaultDailyRetentionDays)
	}
	if got.BatchSize != DefaultPurgeBatch {
		t.Fatalf("BatchSize = %d, want %d", got.BatchSize, DefaultPurgeBatch)
	}
	if got.MaxBatches != DefaultMaxBatches {
		t.Fatalf("MaxBatches = %d, want %d", got.MaxBatches, DefaultMaxBatches)
	}
	if got.Now == nil {
		t.Fatal("Now = nil, want a default clock")
	}

	// The detail window must never outlive the aggregate it rolls up to.
	clamped := PurgeOptions{RetentionDays: 90, DailyRetentionDays: 10}.normalize()
	if clamped.DailyRetentionDays != 90 {
		t.Fatalf("DailyRetentionDays = %d, want it clamped up to 90", clamped.DailyRetentionDays)
	}

	explicit := PurgeOptions{
		RetentionDays:      7,
		DailyRetentionDays: 90,
		BatchSize:          100,
		MaxBatches:         3,
		Now:                func() time.Time { return time.Unix(0, 0) },
	}.normalize()
	if explicit.RetentionDays != 7 || explicit.DailyRetentionDays != 90 ||
		explicit.BatchSize != 100 || explicit.MaxBatches != 3 {
		t.Fatalf("normalize overwrote explicit options: %+v", explicit)
	}
}

// The retention window has to stay clear of the window the Traffic UI
// reads, or a purge silently truncates the leftmost bars of the chart.
func TestDefaultRetentionCoversTheReadWindow(t *testing.T) {
	if DefaultRetentionDays <= DefaultWindowDays {
		t.Fatalf("DefaultRetentionDays = %d, must exceed DefaultWindowDays = %d",
			DefaultRetentionDays, DefaultWindowDays)
	}
	if DefaultDailyRetentionDays < DefaultRetentionDays {
		t.Fatalf("DefaultDailyRetentionDays = %d, must be at least DefaultRetentionDays = %d",
			DefaultDailyRetentionDays, DefaultRetentionDays)
	}
}
