// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lifecycle owns the post-creation mutations of a repo: rename,
// transfer, archive/unarchive, visibility flip, soft-delete, restore,
// and hard-delete. Every operation is a single function with strict
// ordering of DB and FS work; recovery semantics are documented at
// each call site.
//
// The package is policy-agnostic — callers (web handlers, the
// hard-delete worker) are expected to have already passed the request
// through policy.Can with the appropriate Action. Lifecycle's job is
// to *execute* the change correctly, not decide who's allowed.
package lifecycle

import (
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

// Deps wires the orchestrator. Construct once and pass to every Op.
// All Ops are stateless w.r.t. Deps so a single value can serve many
// concurrent requests.
type Deps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Audit  *audit.Recorder
	Logger *slog.Logger
	Now    func() time.Time
}

// now returns deps.Now() or wall time. Tests pin Now for determinism.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Errors surfaced by lifecycle ops. Handlers map these to HTTP status
// codes and friendly user messages.
var (
	ErrInvalidName       = errors.New("lifecycle: invalid name")
	ErrReservedName      = errors.New("lifecycle: name is reserved")
	ErrNameTaken         = errors.New("lifecycle: name already taken on owner")
	ErrRenameRateLimited = errors.New("lifecycle: rename rate limit exceeded")
	ErrSameName          = errors.New("lifecycle: new name equals current")
	ErrTransferToSelf    = errors.New("lifecycle: transfer recipient is the same owner")
	ErrTransferTerminal  = errors.New("lifecycle: transfer no longer pending")
	ErrTransferExpired   = errors.New("lifecycle: transfer expired")
	ErrAlreadyArchived   = errors.New("lifecycle: repo is already archived")
	ErrNotArchived       = errors.New("lifecycle: repo is not archived")
	ErrAlreadyPaused     = errors.New("lifecycle: repo is already paused")
	ErrNotPaused         = errors.New("lifecycle: repo is not paused")
	// ErrCannotPauseArchived covers the case where an owner tries to
	// pause an archived repo. The DB constraint would also reject
	// this, but the handler error path looks cleaner if we catch it
	// before the round-trip.
	ErrCannotPauseArchived = errors.New("lifecycle: cannot pause an archived repo; unarchive first")
	ErrAlreadyDeleted      = errors.New("lifecycle: repo is already soft-deleted")
	ErrNotDeleted          = errors.New("lifecycle: repo is not soft-deleted")
	ErrPastGrace           = errors.New("lifecycle: soft-delete grace expired; restore unavailable")
)

// renameRateLimitWindow / renameRateLimitMax mirror the spec's lean.
// Adjust together; the SQL counts redirects within the window.
const (
	renameRateLimitWindow = 30 * 24 * time.Hour
	renameRateLimitMax    = 5

	// transferTTL is the time a recipient has to accept before the
	// offer auto-expires.
	transferTTL = 7 * 24 * time.Hour

	// softDeleteGrace is the window between soft-delete and the worker
	// hard-deleting. Mirrored in the SQL `interval '7 days'`.
	softDeleteGrace = 7 * 24 * time.Hour
)
