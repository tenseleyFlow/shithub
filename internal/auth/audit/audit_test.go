// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func pgInt(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }

func TestRecord_RoundTrip(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	q := usersdb.New()

	// Seed a user so actor_id FK is satisfied.
	user, err := q.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: "$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	r := NewRecorder()
	if err := r.Record(ctx, pool, user.ID, Action2FAEnabled, TargetUser, user.ID, map[string]any{
		"recovery_count": 10,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := q.ListAuditLogForTarget(ctx, pool, usersdb.ListAuditLogForTargetParams{
		TargetType: string(TargetUser),
		TargetID:   pgInt(user.ID),
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListAuditLogForTarget: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Action != string(Action2FAEnabled) {
		t.Fatalf("action = %q, want %q", rows[0].Action, Action2FAEnabled)
	}
	var meta map[string]any
	if err := json.Unmarshal(rows[0].Meta, &meta); err != nil {
		t.Fatalf("meta JSON: %v", err)
	}
	if meta["recovery_count"] == nil {
		t.Fatalf("meta missing recovery_count: %v", meta)
	}
}

func TestRecord_RejectsEmpty(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	r := NewRecorder()
	if err := r.Record(context.Background(), pool, 0, "", TargetUser, 0, nil); err == nil {
		t.Fatal("expected error for empty action")
	}
	if err := r.Record(context.Background(), pool, 0, Action2FAEnabled, "", 0, nil); err == nil {
		t.Fatal("expected error for empty target_type")
	}
}
