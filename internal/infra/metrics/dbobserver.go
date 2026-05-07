// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ObserveDBPool starts a goroutine that periodically refreshes the pgx
// pool gauges. The goroutine exits when ctx is canceled.
func ObserveDBPool(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	if pool == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				stat := pool.Stat()
				DBConnsAcquired.Set(float64(stat.AcquiredConns()))
				DBConnsIdle.Set(float64(stat.IdleConns()))
				DBConnsTotal.Set(float64(stat.TotalConns()))
			}
		}
	}()
}
