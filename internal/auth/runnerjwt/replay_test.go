// SPDX-License-Identifier: AGPL-3.0-or-later

package runnerjwt_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
)

func TestConsumeRecordsClaims(t *testing.T) {
	claims := runnerjwt.Claims{
		Sub:    "runner:7",
		JobID:  11,
		RunID:  13,
		RepoID: 17,
		Exp:    time.Unix(1000, 0).Unix(),
		JTI:    "0123456789abcdef",
	}
	db := &fakeReplayDB{row: fakeReplayRow{}}

	if err := runnerjwt.Consume(context.Background(), db, claims); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if len(db.args) != 6 {
		t.Fatalf("args length: got %d, want 6", len(db.args))
	}
	if db.args[0] != claims.JTI || db.args[1] != int64(7) ||
		db.args[2] != claims.JobID || db.args[3] != claims.RunID || db.args[4] != claims.RepoID {
		t.Fatalf("unexpected args: %#v", db.args)
	}
	expiresAt, ok := db.args[5].(pgtype.Timestamptz)
	if !ok {
		t.Fatalf("expires_at arg type: got %T", db.args[5])
	}
	if !expiresAt.Valid || expiresAt.Time.Unix() != claims.Exp {
		t.Fatalf("expires_at: got %#v, want unix %d", expiresAt, claims.Exp)
	}
}

func TestConsumeMapsNoRowsToReplay(t *testing.T) {
	claims := runnerjwt.Claims{
		Sub:    "runner:7",
		JobID:  11,
		RunID:  13,
		RepoID: 17,
		Exp:    time.Unix(1000, 0).Unix(),
		JTI:    "0123456789abcdef",
	}
	db := &fakeReplayDB{row: fakeReplayRow{err: pgx.ErrNoRows}}

	if err := runnerjwt.Consume(context.Background(), db, claims); !errors.Is(err, runnerjwt.ErrReplay) {
		t.Fatalf("Consume replay: got %v, want ErrReplay", err)
	}
}

func TestConsumeRejectsInvalidClaimsBeforeDB(t *testing.T) {
	db := &fakeReplayDB{row: fakeReplayRow{}}
	err := runnerjwt.Consume(context.Background(), db, runnerjwt.Claims{
		Sub:    "user:7",
		JobID:  11,
		RunID:  13,
		RepoID: 17,
		Exp:    time.Unix(1000, 0).Unix(),
		JTI:    "0123456789abcdef",
	})
	if !errors.Is(err, runnerjwt.ErrInvalidClaims) {
		t.Fatalf("Consume invalid claims: got %v, want ErrInvalidClaims", err)
	}
	if db.calls != 0 {
		t.Fatalf("DB called for invalid claims: %d", db.calls)
	}
}

type fakeReplayDB struct {
	row   pgx.Row
	args  []any
	calls int
}

func (f *fakeReplayDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeReplayDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func (f *fakeReplayDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.calls++
	f.args = args
	return f.row
}

type fakeReplayRow struct {
	err error
}

func (r fakeReplayRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = "0123456789abcdef"
	*dest[1].(*int64) = 7
	*dest[2].(*int64) = 11
	*dest[3].(*int64) = 13
	*dest[4].(*int64) = 17
	*dest[5].(*pgtype.Timestamptz) = pgtype.Timestamptz{Time: time.Unix(1000, 0), Valid: true}
	*dest[6].(*pgtype.Timestamptz) = pgtype.Timestamptz{Time: time.Unix(900, 0), Valid: true}
	return nil
}
