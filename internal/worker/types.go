// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker drives the Postgres-backed job queue introduced in
// S14. The pool dispatches one Job per goroutine, claiming rows via
// FOR UPDATE SKIP LOCKED so concurrent workers don't double-process.
//
// Job kinds, their payload schema, and their handlers live alongside
// this package in sub-packages (jobs/<kind>.go). The pool itself is
// kind-agnostic: a Handler is just a func that takes a payload and
// returns nil-or-error.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Kind is the canonical name of a job. Use lowercase letters and
// colon-separated namespaces (e.g. "push:process", "repo:size_recalc").
// Kind doubles as the dispatch index — workers query
// `WHERE kind = $1` so adding new kinds doesn't disturb existing ones.
type Kind string

// Built-in kinds shipped in S14.
const (
	KindPushProcess    Kind = "push:process"
	KindRepoSizeRecalc Kind = "repo:size_recalc"
	KindJobsPurge      Kind = "jobs:purge_completed"
)

// S16 lifecycle housekeeping kind.
const (
	KindLifecycleSweep Kind = "lifecycle:sweep"
)

// S22 pull-request kinds. Synchronize refreshes commits + files after
// a head-side push; mergeability runs the merge-tree probe.
//
// (No async-merge kind: the POST .../merge handler executes the merge
// synchronously so the redirect lands on the merged state. If we add
// async merging later — for very large repos — re-introduce a
// KindPRMerge here and a matching jobs.PRMerge handler.)
const (
	KindPRSynchronize  Kind = "pr:synchronize"
	KindPRMergeability Kind = "pr:mergeability"
)

// NotifyChannel is the Postgres LISTEN/NOTIFY channel the pool subscribes
// to so it wakes up immediately when a job is enqueued, instead of
// polling. Callers wrapping enqueue in a tx must NOTIFY inside the
// same tx so the notification only fires on commit.
const NotifyChannel = "shithub_jobs"

// Handler runs one job. The framework supplies the raw JSON payload;
// handlers Unmarshal into their own typed schema. A nil error reports
// success and the job is marked completed; any non-nil error triggers
// the backoff/retry path. ErrPoison is the explicit "do not retry" signal
// — useful when the input is malformed and retrying can't help.
type Handler func(ctx context.Context, payload json.RawMessage) error

// ErrPoison wraps a handler error that should NOT be retried. The pool
// jumps the job straight to MarkJobFailed instead of rescheduling.
var ErrPoison = errors.New("worker: poison job")

// PoisonError wraps cause as a poison error. The cause is preserved in
// last_error for operator inspection.
func PoisonError(cause error) error {
	return fmt.Errorf("%w: %v", ErrPoison, cause)
}

// Backoff returns the delay before retrying a job that is about to be
// rescheduled. The formula is `30s * 2^attempts` capped at 1 hour, with
// ±20% jitter so a fleet doesn't synchronize retries on a sibling
// dependency outage.
//
// `attempts` is the number of attempts already made (1-indexed: the
// just-failed attempt counts as 1).
func Backoff(attempts int, jitter func() float64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	const (
		base = 30 * time.Second
		cap_ = time.Hour
	)
	// Compute base * 2^(attempts-1), guarding against overflow.
	d := base
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= cap_ {
			d = cap_
			break
		}
	}
	if d > cap_ {
		d = cap_
	}
	if jitter != nil {
		// jitter() in [0,1) → multiplier in [0.8, 1.2)
		mult := 0.8 + 0.4*jitter()
		d = time.Duration(float64(d) * mult)
	}
	return d
}
