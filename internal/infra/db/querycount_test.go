// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestQueryCounter_IncrementsOnEveryQuery(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := db.WithCounter(context.Background())

	// Three trivial queries against the live pool.
	for i := 0; i < 3; i++ {
		var n int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			t.Fatalf("SELECT 1: %v", err)
		}
	}
	if got := db.FromContext(ctx); got != 3 {
		t.Errorf("FromContext = %d; want 3", got)
	}
}

func TestQueryCounter_NilCounterIsZeroAndSafe(t *testing.T) {
	t.Parallel()
	if got := db.FromContext(context.Background()); got != 0 {
		t.Errorf("FromContext on bare ctx = %d; want 0", got)
	}
}
