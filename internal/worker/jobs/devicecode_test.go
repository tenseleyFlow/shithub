// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

// TestDeviceAuthorizationSweep covers remediation #5: the daily sweep
// deletes terminal/expired device_authorizations rows past the 24h
// forensics window and leaves everything else alone. Insert three
// rows: one fresh (in-window), one just-expired (still in forensics),
// one ancient (past forensics). Run the handler; assert only the
// ancient row is gone.
func TestDeviceAuthorizationSweep(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	q := usersdb.New()
	ctx := context.Background()

	// All three rows go in via the insert query so the schema check
	// constraints (octet_length(device_code_hash) = 32, etc.) hold.
	mkHash := func(b byte) []byte {
		h := make([]byte, 32)
		for i := range h {
			h[i] = b
		}
		return h
	}
	mkRow := func(t *testing.T, codeByte byte, userCode string, expires time.Time) int64 {
		t.Helper()
		row, err := q.InsertDeviceAuthorization(ctx, pool, usersdb.InsertDeviceAuthorizationParams{
			DeviceCodeHash:  mkHash(codeByte),
			UserCode:        userCode,
			ClientID:        "shithub-cli",
			Scopes:          []string{"user:read"},
			IntervalSeconds: 5,
			ExpiresAt:       pgtype.Timestamptz{Time: expires, Valid: true},
		})
		if err != nil {
			t.Fatalf("InsertDeviceAuthorization: %v", err)
		}
		return row.ID
	}
	fresh := mkRow(t, 0x11, "FRES-HXXX", time.Now().Add(10*time.Minute))
	recent := mkRow(t, 0x22, "RECT-XXXX", time.Now().Add(-1*time.Hour))   // expired but within forensics
	ancient := mkRow(t, 0x33, "ANCT-XXXX", time.Now().Add(-48*time.Hour)) // past forensics

	handler := jobs.DeviceAuthorizationSweep(jobs.DeviceAuthorizationSweepDeps{Pool: pool})
	if err := handler(ctx, nil); err != nil {
		t.Fatalf("sweep handler: %v", err)
	}

	exists := func(id int64) bool {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM device_authorizations WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count for id=%d: %v", id, err)
		}
		return n == 1
	}
	if !exists(fresh) {
		t.Errorf("fresh row was swept (should still be in-window)")
	}
	if !exists(recent) {
		t.Errorf("recent row was swept (should still be within forensics window)")
	}
	if exists(ancient) {
		t.Errorf("ancient row survived sweep (should be deleted past forensics)")
	}
}
