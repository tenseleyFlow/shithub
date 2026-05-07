// SPDX-License-Identifier: AGPL-3.0-or-later

package pat

import (
	"sync"
	"time"
)

// Debouncer suppresses repeat last-used updates for a given token within
// a time window. The auth middleware calls ShouldTouch before issuing
// the DB write so 100 requests/sec hitting the same token don't burn
// 100 UPDATEs on user_tokens.
//
// Eviction is a simple cap-based sweep: once the map exceeds maxEntries,
// we drop entries older than the window. Acceptable for our scale —
// bounded by the active-PAT count, not the request rate.
type Debouncer struct {
	mu         sync.Mutex
	last       map[int64]time.Time
	window     time.Duration
	maxEntries int
	now        func() time.Time
}

// NewDebouncer returns a Debouncer with a 60-second default window and a
// 4096-entry cap. The defaults are right for our launch scale; expose as
// fields (NOT as constructor args) if a test needs to override them.
func NewDebouncer(window time.Duration) *Debouncer {
	if window <= 0 {
		window = 60 * time.Second
	}
	return &Debouncer{
		last:       make(map[int64]time.Time),
		window:     window,
		maxEntries: 4096,
		now:        time.Now,
	}
}

// ShouldTouch reports whether the auth middleware should issue a
// last-used DB write for tokenID right now. Returns true at most once per
// (tokenID, window) pair — subsequent calls within the window return
// false. Always thread-safe.
func (d *Debouncer) ShouldTouch(tokenID int64) bool {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.last[tokenID]; ok && now.Sub(last) < d.window {
		return false
	}
	d.last[tokenID] = now
	if len(d.last) > d.maxEntries {
		d.evictStaleLocked(now)
	}
	return true
}

func (d *Debouncer) evictStaleLocked(now time.Time) {
	for k, t := range d.last {
		if now.Sub(t) >= d.window {
			delete(d.last, k)
		}
	}
}

// Forget removes tokenID from the debouncer's memory — useful when a
// token is revoked, so a subsequent (unauthenticated) lookup with the
// same id can't accidentally inherit a stale "recently touched" state.
func (d *Debouncer) Forget(tokenID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, tokenID)
}
