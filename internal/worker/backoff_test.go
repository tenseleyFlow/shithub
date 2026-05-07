// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/worker"
)

func TestBackoff_Doubles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 480 * time.Second},
		{6, 960 * time.Second},
		{7, 1920 * time.Second},
		{8, time.Hour},  // capped
		{10, time.Hour}, // still capped
	}
	for _, c := range cases {
		got := worker.Backoff(c.attempts, nil)
		if got != c.want {
			t.Errorf("Backoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

func TestBackoff_JitterStaysInBand(t *testing.T) {
	t.Parallel()
	const attempts = 4
	base := worker.Backoff(attempts, nil)
	low := time.Duration(float64(base) * 0.8)
	high := time.Duration(float64(base) * 1.2)
	for i := 0; i < 100; i++ {
		j := float64(i) / 100.0
		got := worker.Backoff(attempts, func() float64 { return j })
		if got < low || got >= high {
			t.Fatalf("jitter %v: got %v, want in [%v, %v)", j, got, low, high)
		}
	}
}

func TestBackoff_NonPositiveAttemptsClampsToOne(t *testing.T) {
	t.Parallel()
	if got := worker.Backoff(0, nil); got != 30*time.Second {
		t.Errorf("Backoff(0) = %v, want 30s", got)
	}
	if got := worker.Backoff(-5, nil); got != 30*time.Second {
		t.Errorf("Backoff(-5) = %v, want 30s", got)
	}
}
