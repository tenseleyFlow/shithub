// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"testing"
	"time"
)

func TestTicksBetween(t *testing.T) {
	tests := []struct {
		name  string
		tick  time.Duration
		every time.Duration
		want  int
	}{
		{"production cadence", 15 * time.Second, 5 * time.Minute, 20},
		{"rounds up", 7 * time.Second, 5 * time.Minute, 43},
		{"slow interval equals tick", 15 * time.Second, 15 * time.Second, 1},
		{"slow interval below tick", time.Minute, 5 * time.Second, 1},
		{"zero tick", 0, 5 * time.Minute, 1},
		{"negative tick", -time.Second, 5 * time.Minute, 1},
		{"zero slow interval", 15 * time.Second, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticksBetween(tc.tick, tc.every); got != tc.want {
				t.Fatalf("ticksBetween(%v, %v) = %d, want %d", tc.tick, tc.every, got, tc.want)
			}
		})
	}
}

// The point of the split is that the expensive refresh must not keep pace
// with the cheap one, so assert the exact call counts over a run of ticks.
func TestObserveActionsLoopRunsSlowRefreshEveryNTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time)
	var fast, slow int
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeActionsLoop(ctx, ticks, 4,
			func(context.Context) { fast++ },
			func(context.Context) { slow++ },
		)
	}()

	// Both refreshes run once before the first tick so the gauges are not
	// empty for the first interval; 12 ticks at slowEvery=4 add 3 slow runs.
	for i := 0; i < 12; i++ {
		ticks <- time.Now()
	}
	// Closing the channel makes the loop return, which orders the counter
	// writes before the reads below.
	close(ticks)
	<-done

	if fast != 13 {
		t.Fatalf("fast refreshes = %d, want 13 (initial plus one per tick)", fast)
	}
	if slow != 4 {
		t.Fatalf("slow refreshes = %d, want 4 (initial plus one per 4 ticks)", slow)
	}
}

func TestObserveActionsLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	var fast, slow int
	go func() {
		defer close(done)
		observeActionsLoop(ctx, ticks, 2,
			func(context.Context) { fast++ },
			func(context.Context) { slow++ },
		)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observeActionsLoop did not return after context cancel")
	}
	if fast != 1 || slow != 1 {
		t.Fatalf("fast=%d slow=%d, want the single up-front refresh of each", fast, slow)
	}
}

func TestObserveActionsLoopClampsSlowEvery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time)
	var fast, slow int
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeActionsLoop(ctx, ticks, 0,
			func(context.Context) { fast++ },
			func(context.Context) { slow++ },
		)
	}()
	for i := 0; i < 3; i++ {
		ticks <- time.Now()
	}
	close(ticks)
	<-done

	if fast != 4 || slow != 4 {
		t.Fatalf("fast=%d slow=%d, want 4 and 4 (slowEvery clamped to 1)", fast, slow)
	}
}
