// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{8, 64 * time.Minute},
	}
	for _, tc := range cases {
		got := Backoff(tc.attempt, nil)
		if got != tc.want {
			t.Errorf("Backoff(%d) = %v; want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffCapsAt24Hours(t *testing.T) {
	got := Backoff(50, nil)
	if got != BackoffMax {
		t.Fatalf("Backoff(50) = %v; want %v", got, BackoffMax)
	}
}

func TestBackoffJitterStaysWithinTwentyPercent(t *testing.T) {
	base := Backoff(3, nil) // 2 minutes
	for j := 0.0; j < 1.0; j += 0.05 {
		jit := j
		got := Backoff(3, func() float64 { return jit })
		min := time.Duration(float64(base) * 0.79)
		max := time.Duration(float64(base) * 1.21)
		if got < min || got > max {
			t.Errorf("Backoff(3, jitter=%.2f) = %v; want in [%v, %v]", jit, got, min, max)
		}
	}
}

func TestBackoffClampsZeroAttempt(t *testing.T) {
	got := Backoff(0, nil)
	if got != BackoffBase {
		t.Fatalf("Backoff(0) = %v; want %v", got, BackoffBase)
	}
}
