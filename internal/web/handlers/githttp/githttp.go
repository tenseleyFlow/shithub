// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githttp wires the smart-HTTP git protocol routes:
// info/refs + git-upload-pack + git-receive-pack. Each request shells
// out to canonical `git` plumbing — we don't reimplement the protocol.
//
// This file owns the handler struct + Deps + Mounting; the route bodies
// live in handler_*.go and the credential resolver lives in auth.go.
package githttp

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// Deps wires the git-HTTP handler set.
type Deps struct {
	Logger *slog.Logger
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	// MaxPushBytes is the hard cap on the git-receive-pack request body.
	// Defaults to 2 GiB when zero.
	MaxPushBytes int64
}

// Handlers is the registered handler set. Construct via New.
type Handlers struct {
	d  Deps
	uq *usersdb.Queries
	rq *reposdb.Queries
	oq *orgsdb.Queries
}

// DefaultMaxPushBytes is the spec-recommended cap.
const DefaultMaxPushBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Pool == nil {
		return nil, errors.New("githttp: nil Pool")
	}
	if d.RepoFS == nil {
		return nil, errors.New("githttp: nil RepoFS")
	}
	if d.MaxPushBytes == 0 {
		d.MaxPushBytes = DefaultMaxPushBytes
	}
	return &Handlers{d: d, uq: usersdb.New(), rq: reposdb.New(), oq: orgsdb.New()}, nil
}
