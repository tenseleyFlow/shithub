// SPDX-License-Identifier: AGPL-3.0-or-later

package cronworkflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	crondb "github.com/tenseleyFlow/shithub/internal/cronworkflow/sqlc"
)

// MaxDispatchesPerUser caps how many schedules a Pro user can hold.
// Bounds the sweep budget and keeps a runaway script from filling the
// table. Reasonable default per the campaign overview; PR 13c's
// create handler enforces.
const MaxDispatchesPerUser = 20

// Errors surfaced by the Store / Sweep callers.
var (
	ErrNotFound       = errors.New("cronworkflow: not found")
	ErrTooManyForUser = fmt.Errorf("cronworkflow: too many schedules (max %d)", MaxDispatchesPerUser)
	ErrEmptyWorkflow  = errors.New("cronworkflow: workflow file must be non-empty")
	ErrEmptyRef       = errors.New("cronworkflow: ref must be non-empty")
)

// Deps wires the store.
type Deps struct {
	Pool *pgxpool.Pool
}

// Dispatch is the in-package view of one cron dispatch row.
type Dispatch struct {
	ID             int64
	UserID         int64
	RepoID         int64
	WorkflowFile   string
	Ref            string
	CronExpr       string
	NextFireAt     time.Time
	LastFireAt     time.Time
	LastFireStatus string
	LastFireError  string
	Disabled       bool
}

// CreateInput is the create-time input shape. PR 13c's REST handler
// validates the workflow file exists at ref; this layer trusts the
// caller. ParseExpr is run here so a malformed cron expression
// returns an error before any DB write.
type CreateInput struct {
	UserID       int64
	RepoID       int64
	WorkflowFile string
	Ref          string
	CronExpr     string
}

// Create parses the cron expression, computes the first next_fire_at
// from now, and inserts the row. Returns ErrInvalidCronExpr when the
// expression is bad.
func (d Deps) Create(ctx context.Context, in CreateInput) (Dispatch, error) {
	if d.Pool == nil {
		return Dispatch{}, errors.New("cronworkflow: Create needs Pool")
	}
	if in.WorkflowFile == "" {
		return Dispatch{}, ErrEmptyWorkflow
	}
	if in.Ref == "" {
		return Dispatch{}, ErrEmptyRef
	}
	next, err := NextTick(in.CronExpr, time.Now())
	if err != nil {
		return Dispatch{}, err
	}
	row, err := crondb.New().CreateCronDispatch(ctx, d.Pool, crondb.CreateCronDispatchParams{
		UserID:       in.UserID,
		RepoID:       in.RepoID,
		WorkflowFile: in.WorkflowFile,
		Ref:          in.Ref,
		CronExpr:     in.CronExpr,
		NextFireAt:   pgtype.Timestamptz{Time: next, Valid: true},
	})
	if err != nil {
		return Dispatch{}, fmt.Errorf("cronworkflow: insert: %w", err)
	}
	return toDispatch(row), nil
}

// GetByID fetches a single row.
func (d Deps) GetByID(ctx context.Context, id int64) (Dispatch, error) {
	row, err := crondb.New().GetCronDispatchByID(ctx, d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dispatch{}, ErrNotFound
		}
		return Dispatch{}, fmt.Errorf("cronworkflow: get: %w", err)
	}
	return toDispatch(row), nil
}

// ListForUser returns every schedule owned by a user.
func (d Deps) ListForUser(ctx context.Context, userID int64) ([]Dispatch, error) {
	rows, err := crondb.New().ListCronDispatchesForUser(ctx, d.Pool, userID)
	if err != nil {
		return nil, fmt.Errorf("cronworkflow: list: %w", err)
	}
	out := make([]Dispatch, len(rows))
	for i, r := range rows {
		out[i] = toDispatch(r)
	}
	return out, nil
}

// Disable flips disabled_at. Sweep skips disabled rows.
func (d Deps) Disable(ctx context.Context, id int64) error {
	return crondb.New().DisableCronDispatch(ctx, d.Pool, id)
}

// Delete removes the row.
func (d Deps) Delete(ctx context.Context, id int64) error {
	return crondb.New().DeleteCronDispatch(ctx, d.Pool, id)
}

func toDispatch(row crondb.UserCronDispatch) Dispatch {
	d := Dispatch{
		ID:             row.ID,
		UserID:         row.UserID,
		RepoID:         row.RepoID,
		WorkflowFile:   row.WorkflowFile,
		Ref:            row.Ref,
		CronExpr:       row.CronExpr,
		NextFireAt:     row.NextFireAt.Time,
		LastFireStatus: string(row.LastFireStatus),
		Disabled:       row.DisabledAt.Valid,
	}
	if row.LastFireAt.Valid {
		d.LastFireAt = row.LastFireAt.Time
	}
	if row.LastFireError.Valid {
		d.LastFireError = row.LastFireError.String
	}
	return d
}
