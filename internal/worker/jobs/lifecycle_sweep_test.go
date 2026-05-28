// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRunLifecycleSweepHardDeletesBoundsStuckRows(t *testing.T) {
	t.Parallel()

	var seen []int64
	err := runLifecycleSweepHardDeletes(
		context.Background(),
		[]int64{10, 20},
		5*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(ctx context.Context, id int64) error {
			seen = append(seen, id)
			if id == 10 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("runLifecycleSweepHardDeletes: %v", err)
	}
	if len(seen) != 2 || seen[0] != 10 || seen[1] != 20 {
		t.Fatalf("seen ids = %v, want [10 20]", seen)
	}
}

func TestRunLifecycleSweepHardDeletesStopsWhenParentCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := runLifecycleSweepHardDeletes(ctx, []int64{10}, time.Second, nil, func(context.Context, int64) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("hard delete called after parent context was canceled")
	}
}
