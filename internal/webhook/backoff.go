// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import "time"

// BackoffBase is the smallest retry delay; doubles per attempt.
const BackoffBase = 30 * time.Second

// BackoffMax caps the delay so a stale subscriber doesn't disappear
// for days at a time. The spec calls for 24 hours.
const BackoffMax = 24 * time.Hour

// Backoff returns the delay before retrying delivery `attempt` (1-
// indexed: the just-failed attempt counts as 1). Applies ±20% jitter
// so a fleet of webhooks pointed at one outage doesn't synchronize
// retries. `jitter` returns a value in [0, 1); pass `nil` for no jitter
// (deterministic schedule, useful in tests).
func Backoff(attempt int, jitter func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= BackoffMax {
			d = BackoffMax
			break
		}
	}
	if d > BackoffMax {
		d = BackoffMax
	}
	if jitter != nil {
		mult := 0.8 + 0.4*jitter()
		d = time.Duration(float64(d) * mult)
	}
	return d
}
