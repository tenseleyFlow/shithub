// SPDX-License-Identifier: AGPL-3.0-or-later

package traffic

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindTrafficPurge is the cron-driven retention sweep over the traffic
// tables. Registered by cmd/shithubd/worker.go, enqueued nightly by
// deploy/systemd/shithubd-cron.service.
const KindTrafficPurge worker.Kind = "traffic:purge"

// Retention windows for the traffic tables.
//
// The per-path, per-referrer and per-visitor tables are the ones that
// grow without bound — one row per distinct path (or referrer, or
// visitor digest) per repo per day, which crawlers inflate hard. The
// Traffic UI only ever reads DefaultWindowDays (14) of them, so
// anything older is dead weight; the window here is deliberately more
// than double that so a purge can never eat a bar the UI would draw.
//
// repo_traffic_daily is different: one row per repo per day, no
// cardinality explosion, and it is the only long-term history the
// project has. It keeps a little over a year so a future
// year-over-year view has something to read, but it is still bounded.
const (
	DefaultRetentionDays      = 30
	DefaultDailyRetentionDays = 400
)

// Batch sizing for the purge loop.
//
// DefaultPurgeBatch is small enough that one DELETE stays well inside a
// normal statement timeout and holds row locks briefly; DefaultMaxBatches
// stops a single run from grinding indefinitely if the backlog is larger
// than expected. The job is idempotent and cron-driven, so a run that
// stops early just leaves the rest for the next night.
const (
	DefaultPurgeBatch = 5000
	DefaultMaxBatches = 2000
)

// PurgeOptions tunes one run. Zero values select the defaults above.
type PurgeOptions struct {
	// RetentionDays applies to repo_traffic_uniques, repo_traffic_paths
	// and repo_traffic_referrers.
	RetentionDays int
	// DailyRetentionDays applies to repo_traffic_daily.
	DailyRetentionDays int
	// BatchSize is the row cap on a single DELETE statement.
	BatchSize int
	// MaxBatches caps the number of DELETEs per table per run.
	MaxBatches int
	// Now is injectable so tests can pin the cutoff. Defaults to time.Now.
	Now func() time.Time
}

func (o PurgeOptions) normalize() PurgeOptions {
	if o.RetentionDays <= 0 {
		o.RetentionDays = DefaultRetentionDays
	}
	if o.DailyRetentionDays <= 0 {
		o.DailyRetentionDays = DefaultDailyRetentionDays
	}
	// A daily window shorter than the detail window would throw away the
	// aggregate while its own source rows were still live, which is never
	// what an operator means.
	if o.DailyRetentionDays < o.RetentionDays {
		o.DailyRetentionDays = o.RetentionDays
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultPurgeBatch
	}
	if o.MaxBatches <= 0 {
		o.MaxBatches = DefaultMaxBatches
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// PurgeResult reports what one run removed.
type PurgeResult struct {
	UniquesDeleted   int64
	PathsDeleted     int64
	ReferrersDeleted int64
	DailyDeleted     int64
	// Remaining is true when at least one table hit MaxBatches, i.e. rows
	// older than the cutoff are still there and the next run has work.
	Remaining bool
}

// Total is the row count deleted across all four tables.
func (r PurgeResult) Total() int64 {
	return r.UniquesDeleted + r.PathsDeleted + r.ReferrersDeleted + r.DailyDeleted
}

// Purge trims the traffic tables to their retention windows.
//
// Every DELETE runs on its own connection outside an explicit
// transaction, so no single statement holds locks for long and a run
// interrupted halfway through leaves the rows it already deleted
// deleted. Re-running is always safe: the cutoff is recomputed from the
// clock and rows inside the window are never touched.
func Purge(ctx context.Context, pool *pgxpool.Pool, opts PurgeOptions) (PurgeResult, error) {
	opts = opts.normalize()

	now := opts.Now().UTC()
	cutoffDay := startDate(now).AddDate(0, 0, -opts.RetentionDays)
	dailyCutoffDay := startDate(now).AddDate(0, 0, -opts.DailyRetentionDays)
	batch := int64(opts.BatchSize)

	q := reposdb.New()
	var res PurgeResult

	// repo_traffic_uniques filters on created_at, which is the column it
	// has an index on; midnight UTC of the cutoff day is the same instant
	// the date comparison would pick.
	deleted, more, err := purgeBatched(ctx, opts.MaxBatches, batch,
		func(ctx context.Context, limit int64) (int64, error) {
			return q.PurgeRepoTrafficUniquesBatch(ctx, pool, reposdb.PurgeRepoTrafficUniquesBatchParams{
				Cutoff:    pgtype.Timestamptz{Time: cutoffDay, Valid: true},
				BatchSize: limit,
			})
		})
	res.UniquesDeleted = deleted
	res.Remaining = res.Remaining || more
	if err != nil {
		return res, fmt.Errorf("purge repo_traffic_uniques: %w", err)
	}

	deleted, more, err = purgeBatched(ctx, opts.MaxBatches, batch,
		func(ctx context.Context, limit int64) (int64, error) {
			return q.PurgeRepoTrafficPathsBatch(ctx, pool, reposdb.PurgeRepoTrafficPathsBatchParams{
				Cutoff:    pgtype.Date{Time: cutoffDay, Valid: true},
				BatchSize: limit,
			})
		})
	res.PathsDeleted = deleted
	res.Remaining = res.Remaining || more
	if err != nil {
		return res, fmt.Errorf("purge repo_traffic_paths: %w", err)
	}

	deleted, more, err = purgeBatched(ctx, opts.MaxBatches, batch,
		func(ctx context.Context, limit int64) (int64, error) {
			return q.PurgeRepoTrafficReferrersBatch(ctx, pool, reposdb.PurgeRepoTrafficReferrersBatchParams{
				Cutoff:    pgtype.Date{Time: cutoffDay, Valid: true},
				BatchSize: limit,
			})
		})
	res.ReferrersDeleted = deleted
	res.Remaining = res.Remaining || more
	if err != nil {
		return res, fmt.Errorf("purge repo_traffic_referrers: %w", err)
	}

	deleted, more, err = purgeBatched(ctx, opts.MaxBatches, batch,
		func(ctx context.Context, limit int64) (int64, error) {
			return q.PurgeRepoTrafficDailyBatch(ctx, pool, reposdb.PurgeRepoTrafficDailyBatchParams{
				Cutoff:    pgtype.Date{Time: dailyCutoffDay, Valid: true},
				BatchSize: limit,
			})
		})
	res.DailyDeleted = deleted
	res.Remaining = res.Remaining || more
	if err != nil {
		return res, fmt.Errorf("purge repo_traffic_daily: %w", err)
	}

	return res, nil
}

// purgeBatched calls del until it reports a short batch — meaning
// nothing older than the cutoff is left — or maxBatches statements have
// run. It returns the rows deleted, whether it stopped on the batch cap
// with work outstanding, and the first error.
//
// The short-batch check is what terminates the loop; without it a table
// whose row count is an exact multiple of the batch size would still
// exit, but only after one extra empty DELETE. That extra round trip is
// cheap and keeps the condition a single comparison.
func purgeBatched(ctx context.Context, maxBatches int, batch int64, del func(context.Context, int64) (int64, error)) (int64, bool, error) {
	if batch <= 0 || maxBatches <= 0 {
		return 0, false, nil
	}
	var total int64
	for i := 0; i < maxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return total, true, err
		}
		n, err := del(ctx, batch)
		total += n
		if err != nil {
			return total, true, err
		}
		if n < batch {
			return total, false, nil
		}
	}
	return total, true, nil
}
