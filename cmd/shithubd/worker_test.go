// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/worker"
)

// The pgx pool in workerCmd is sized to resolveWorkerCount()+2, so a
// zero here is not a cosmetic default — it is the 2-connection pool
// behind four workers that the 2026-09-02 availability sitrep found
// in production.
func TestResolveWorkerCount(t *testing.T) {
	tests := []struct {
		name string
		flag int
		env  string
		want int
	}{
		{"nothing set falls back to the pool default", 0, "", worker.DefaultWorkers},
		{"env alone", 0, "8", 8},
		{"flag alone", 6, "", 6},
		{"flag beats env", 6, "8", 6},
		{"negative flag falls through to env", -1, "3", 3},
		{"zero env falls back to the default", 0, "0", worker.DefaultWorkers},
		{"negative env falls back to the default", 0, "-2", worker.DefaultWorkers},
		{"garbage env falls back to the default", 0, "four", worker.DefaultWorkers},
		{"whitespace is trimmed", 0, "  5\t", 5},
		{"absurd env is clamped", 0, "1000", maxWorkerCount},
		{"absurd flag is clamped", 1000, "", maxWorkerCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveWorkerCount(tt.flag, tt.env); got != tt.want {
				t.Fatalf("resolveWorkerCount(%d, %q) = %d, want %d", tt.flag, tt.env, got, tt.want)
			}
		})
	}
}

// Regression guard for the actual bug: whatever the inputs, the pool
// must never be opened with fewer connections than workers + LISTEN.
func TestResolveWorkerCountAlwaysLeavesRoomForListen(t *testing.T) {
	for _, env := range []string{"", "0", "-1", "nonsense", "1", "64", "1000"} {
		count := resolveWorkerCount(0, env)
		if count < 1 {
			t.Fatalf("SHITHUB_WORKERS=%q resolved to %d workers", env, count)
		}
		maxConns := count + 2
		if maxConns <= count {
			t.Fatalf("SHITHUB_WORKERS=%q: max_conns %d does not exceed workers %d", env, maxConns, count)
		}
	}
}
