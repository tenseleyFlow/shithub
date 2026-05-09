// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fork owns S27's fork orchestration. The DB row for a fork
// is created synchronously in Create; the on-disk `git clone --bare
// --shared` runs out-of-band as a `repo:fork_clone` worker job so
// fork creation returns fast even for large source repos.
//
// Sync (fast-forward fork's default branch from upstream) is the
// other entrypoint here. Cross-fork PR support extends `internal/
// pulls` and lives there; this package is just the fork lifecycle.
package fork

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

// Deps wires this package against the rest of the runtime.
type Deps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Audit  *audit.Recorder
	Logger *slog.Logger
}

// Errors surfaced to handlers.
var (
	ErrNotLoggedIn         = errors.New("fork: login required")
	ErrSourceNotFound      = errors.New("fork: source repo not found")
	ErrSourceNotVisible    = errors.New("fork: source repo not visible to actor")
	ErrTargetNameTaken     = errors.New("fork: target name already exists for owner")
	ErrVisibilityFloor     = errors.New("fork: target visibility cannot exceed source visibility")
	ErrSelfForkSameName    = errors.New("fork: forking into the same owner requires a different name")
	ErrSourceArchived      = errors.New("fork: source repo is archived")
	ErrSourceDeleted       = errors.New("fork: source repo is deleted")
	ErrSyncDiverged        = errors.New("fork: fork has diverged from upstream; sync via your client")
	ErrSyncUpToDate        = errors.New("fork: already up to date")
	ErrSyncDefaultsMissing = errors.New("fork: source or fork default branch is empty")
	ErrSyncRefRaced        = errors.New("fork: ref changed concurrently; retry")
	ErrForkNotInitialized  = errors.New("fork: fork is still being prepared")
)
