// SPDX-License-Identifier: AGPL-3.0-or-later

// Package social owns S26's stars / watches / events surface. The
// package is read-mostly from the rest of the runtime: notifications
// fan-out (S29) reads `watches` for routing, the activity feed
// (post-MVP) reads `domain_events` for public timelines, and the
// repo profile reads cached counts. Mutations come through the
// orchestrator entrypoints below.
package social

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
)

// Deps wires the package against the rest of the runtime. Pool is
// required. Limiter governs the per-user star/unstar rate cap (spec
// pitfall: trending-manipulation abuse vector). Logger is optional
// (falls back to discarding when nil). Audit is optional; when set,
// state-changing operations record an audit row.
type Deps struct {
	Pool    *pgxpool.Pool
	Limiter *throttle.Limiter
	Logger  *slog.Logger
	Audit   *audit.Recorder
}

// Errors surfaced to handlers.
var (
	ErrNotLoggedIn      = errors.New("social: login required")
	ErrInvalidWatchLevel = errors.New("social: watch level must be all, participating, or ignore")
	ErrStarRateLimit    = errors.New("social: star/unstar rate limit exceeded")
)
