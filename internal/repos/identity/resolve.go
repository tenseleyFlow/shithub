// SPDX-License-Identifier: AGPL-3.0-or-later

// Package identity resolves a commit author's email to a shithub user.
// Used by the commits-list, single-commit, and blame views to render
// a clickable username + avatar instead of the raw "Name <email>" git
// produces.
//
// Resolution rule: match the author email against ANY verified row in
// user_emails — users frequently commit with work / personal emails.
// A match wires the username, display name, and avatar URL; a miss
// falls back to a deterministic identicon hash of the email.
package identity

import (
	"context"
	"crypto/md5" //nolint:gosec // identicon hash, not a security primitive
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// Resolved is the result of looking up one author email.
//
// User=true means the email matched a verified row; the Username +
// DisplayName fields are populated and AvatarURL points at the user's
// avatar route.
//
// User=false means the email was unknown OR matched only an
// unverified row. The handler should render the raw author name with
// a deterministic identicon fallback (IdenticonSeed).
type Resolved struct {
	User           bool
	UserID         int64
	Username       string
	DisplayName    string
	AvatarURL      string
	IdenticonSeed  string
}

// Resolver resolves emails. Per-request memoization keeps a commits-
// list page (30 commits, often one author) to one DB query. Construct
// per request; do not share across requests.
type Resolver struct {
	pool   *pgxpool.Pool
	q      *usersdb.Queries
	cache  map[string]Resolved
	mu     sync.Mutex
}

// New returns a fresh resolver tied to a pgx pool. Pass nil pool only
// in tests that don't expect any DB hits.
func New(pool *pgxpool.Pool) *Resolver {
	return &Resolver{
		pool:  pool,
		q:     usersdb.New(),
		cache: make(map[string]Resolved, 8),
	}
}

// Resolve returns the rendered identity for an email. Empty input
// returns an unresolved Resolved with an empty seed.
func (r *Resolver) Resolve(ctx context.Context, email string) Resolved {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return Resolved{}
	}
	r.mu.Lock()
	if cached, ok := r.cache[email]; ok {
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()

	seed := identiconSeed(email)
	out := Resolved{IdenticonSeed: seed}

	if r.pool != nil {
		em, err := r.q.GetUserEmailByAddress(ctx, r.pool, email)
		if err == nil && em.Verified {
			user, uerr := r.q.GetUserByID(ctx, r.pool, em.UserID)
			if uerr == nil && !user.SuspendedAt.Valid && !user.DeletedAt.Valid {
				out.User = true
				out.UserID = user.ID
				out.Username = user.Username
				out.DisplayName = user.DisplayName
				out.AvatarURL = "/avatars/" + user.Username
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// log? handler-side concern; we just fall through.
		}
	}

	r.mu.Lock()
	r.cache[email] = out
	r.mu.Unlock()
	return out
}

// identiconSeed returns the md5 hex of the lowercased+trimmed email,
// matching the gravatar/identicon convention. Identicon rendering
// itself happens client-side or via the avatars route — this function
// only produces the input.
func identiconSeed(email string) string {
	h := md5.Sum([]byte(email)) //nolint:gosec
	return hex.EncodeToString(h[:])
}
