// SPDX-License-Identifier: AGPL-3.0-or-later

// Package checks owns CI check-suite + check-run orchestration. The
// API layer (internal/web/handlers/api/checks.go) calls into Create
// and Update; the merge gate at internal/pulls/pulls.go calls
// EvaluateRequiredChecks.
//
// No work in this package executes user code — shithub is a
// receiver of check status from external systems (and, post-MVP,
// shithub Actions). The API surface is GitHub-shaped so existing CI
// adapters port cleanly.
package checks

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps wires the package against the runtime.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Output is the JSON shape stored in `check_runs.output`. Matches
// GitHub's `output` shape: title + summary + text. Bodies are bounded
// at the orchestrator (256 KiB on text, 64 KiB on summary) per spec.
type Output struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Text    string `json:"text,omitempty"`
}

// MaxOutputTextBytes / MaxOutputSummaryBytes cap the output payload
// per spec section "Open questions".
const (
	MaxOutputTextBytes    = 256 * 1024
	MaxOutputSummaryBytes = 64 * 1024
)

// Errors surfaced to API + handlers.
var (
	ErrEmptyName              = errors.New("checks: name is required")
	ErrNameTooLong            = errors.New("checks: name too long (max 200)")
	ErrInvalidStatus          = errors.New("checks: status must be queued, in_progress, completed, or pending")
	ErrInvalidConclusion      = errors.New("checks: invalid conclusion")
	ErrCompletedNeedsConclusion = errors.New("checks: completed status requires conclusion")
	ErrOutputTextTooLarge     = errors.New("checks: output.text exceeds 256 KiB cap")
	ErrOutputSummaryTooLarge  = errors.New("checks: output.summary exceeds 64 KiB cap")
	ErrShortHeadSHA           = errors.New("checks: head_sha must be at least 7 hex chars")
	ErrCheckRunNotFound       = errors.New("checks: run not found")
	ErrSuiteNotFound          = errors.New("checks: suite not found")
)

// validStatus / validConclusion mirror the Postgres enums.
func validStatus(s string) bool {
	switch s {
	case "queued", "in_progress", "completed", "pending":
		return true
	}
	return false
}

func validConclusion(s string) bool {
	switch s {
	case "success", "failure", "neutral", "cancelled",
		"skipped", "timed_out", "action_required", "stale":
		return true
	}
	return false
}
