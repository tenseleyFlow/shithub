// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cronworkflow implements PRO-EXT01-13b — per-user cron-
// scheduled workflow_dispatch. A schedule is one (repo, workflow_file,
// ref, cron_expr) row owned by a user; the sweep claims due rows,
// fires them through internal/actions/trigger.Enqueue with
// EventKind=schedule, and advances next_fire_at.
//
// Cron syntax is the standard 5-field crontab in UTC (matches the
// GitHub Actions `schedule:` semantic so users transitioning from a
// committed workflow_dispatch schedule don't have to re-think
// expressions). Parsed at create time by robfig/cron/v3.
package cronworkflow

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser is the standard 5-field crontab parser. We deliberately
// do NOT enable seconds-resolution or descriptors like @hourly — the
// goal is exact parity with the `schedule:` field semantic in
// workflow files, where the 5-field shape is the load-bearing API
// contract.
//
// var rather than const because cron.NewParser returns a struct
// containing function values.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ErrInvalidCronExpr signals an unparseable expression. Callers (the
// create handler) map this to a 400 with the user-visible reason.
var ErrInvalidCronExpr = errors.New("cronworkflow: invalid cron expression")

// ParseExpr validates a cron expression and returns the parsed
// schedule. Used at create time so a malformed expression never
// reaches the DB.
func ParseExpr(expr string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCronExpr, err)
	}
	return sched, nil
}

// NextTick is the wrapper the sweep + create paths share. Given an
// expression and a "from" time, it returns the next fire time in UTC.
//
// The from-arg is plumbed (rather than using time.Now() inline) so
// the sweep can advance based on the row's previous next_fire_at
// rather than now() — this keeps the schedule on a deterministic
// cadence even if the sweep lagged by minutes. Without this, every
// missed tick would silently shift the schedule forward.
func NextTick(expr string, from time.Time) (time.Time, error) {
	sched, err := ParseExpr(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from.UTC()), nil
}
