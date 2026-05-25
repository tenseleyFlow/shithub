// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets owns the orchestrator over `workflow_secrets`. The
// table holds AEAD-encrypted blobs (ChaCha20Poly1305 via
// internal/auth/secretbox); plaintext never lives in postgres.
//
// Four scopes share the table:
//
//   - **Repo secrets**: visible only to workflows running in that repo.
//   - **Org secrets**: visible to workflows running in any of the org's
//     repos. Repo-scoped secrets shadow org secrets with the same name
//     (resolution order is repo → org).
//   - **User secrets**: visible to workflows running in repos owned by
//     that user.
//   - **Environment secrets**: visible only to jobs that declare the
//     matching repo environment; environment-scoped secrets shadow the
//     broader scopes.
//
// The XOR is enforced by a CHECK on the table; the typed Scope here
// is the in-Go mirror. Callers should go through Scope helpers so the
// four-way XOR is not a struct-literal trap.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
)

// Deps wires the store against runtime infra.
type Deps struct {
	Pool   *pgxpool.Pool
	Box    *secretbox.Box
	Logger *slog.Logger
}

// Scope identifies which `workflow_secrets` row family a call targets.
// Construct via RepoScope, OrgScope, UserScope, or EnvironmentScope;
// RepoID, OrgID, UserID, and EnvironmentID are mutually exclusive (the
// table CHECK constraint enforces this server-side).
type Scope struct {
	RepoID        int64
	OrgID         int64
	UserID        int64 // PRO-EXT01-12
	EnvironmentID int64 // SP23
}

// RepoScope returns a repo-scoped Scope. Repo secrets are visible only
// to workflows running in that repo.
func RepoScope(id int64) Scope { return Scope{RepoID: id} }

// OrgScope returns an org-scoped Scope. Org secrets are visible to
// workflows running in any repo owned by the org.
func OrgScope(id int64) Scope { return Scope{OrgID: id} }

// UserScope returns a user-scoped Scope. User secrets are visible to
// workflows running in any repo owned by that user (PRO-EXT01-12).
func UserScope(id int64) Scope { return Scope{UserID: id} }

// EnvironmentScope returns an environment-scoped Scope. Environment
// secrets are visible only to jobs that target the matching repo environment.
func EnvironmentScope(id int64) Scope { return Scope{EnvironmentID: id} }

// IsRepo reports whether the scope addresses a repo. Mutex with all other scopes.
func (s Scope) IsRepo() bool {
	return s.RepoID != 0 && s.OrgID == 0 && s.UserID == 0 && s.EnvironmentID == 0
}

// IsOrg reports whether the scope addresses an org. Mutex with all other scopes.
func (s Scope) IsOrg() bool {
	return s.OrgID != 0 && s.RepoID == 0 && s.UserID == 0 && s.EnvironmentID == 0
}

// IsUser reports whether the scope addresses a user. Mutex with all other scopes.
func (s Scope) IsUser() bool {
	return s.UserID != 0 && s.RepoID == 0 && s.OrgID == 0 && s.EnvironmentID == 0
}

// IsEnvironment reports whether the scope addresses a repo environment. Mutex with all other scopes.
func (s Scope) IsEnvironment() bool {
	return s.EnvironmentID != 0 && s.RepoID == 0 && s.OrgID == 0 && s.UserID == 0
}

// Meta is the public listing shape — no plaintext, no ciphertext.
// The web UI + runner claim path consume Meta when listing names;
// only Get returns the actual decrypted value.
type Meta struct {
	ID              int64
	Name            string
	CreatedByUserID int64 // 0 when null
	CreatedAt       pgtype.Timestamptz
	UpdatedAt       pgtype.Timestamptz
}

// Errors surfaced by the store. Callers (web handlers, runner API)
// map these to HTTP status codes.
var (
	// ErrInvalidScope: zero-or-multiple Scope fields. Programmer error.
	ErrInvalidScope = errors.New("secrets: scope must address exactly one of RepoID, OrgID, UserID, or EnvironmentID")
	// ErrInvalidName: name doesn't match the regex. User-recoverable.
	ErrInvalidName = errors.New("secrets: name must match ^[A-Za-z_][A-Za-z0-9_]*$ and be 1..100 chars")
	// ErrEmptyValue: no zero-length secrets — operators almost
	// certainly mean "delete" if they pass empty.
	ErrEmptyValue = errors.New("secrets: value must be non-empty (use Delete to remove)")
	// ErrNotFound: no row with the given name in this scope.
	ErrNotFound = errors.New("secrets: not found")
)

// nameRe mirrors the workflow_secrets_name_format CHECK in migration
// 0045. Validating parser-side surfaces a user-friendly error before
// the INSERT round-trip.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateName enforces the regex + length cap. Returns ErrInvalidName
// on mismatch.
func validateName(name string) error {
	if len(name) < 1 || len(name) > 100 {
		return ErrInvalidName
	}
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// Set creates or updates a secret in scope. plaintext is encrypted
// with the configured Box before INSERT — the DB never sees the raw
// value. createdBy is the user's ID for audit; 0 when system-driven.
func (d Deps) Set(ctx context.Context, scope Scope, name string, plaintext []byte, createdBy int64) error {
	if !scope.IsRepo() && !scope.IsOrg() && !scope.IsUser() && !scope.IsEnvironment() {
		return ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return err
	}
	if len(plaintext) == 0 {
		return ErrEmptyValue
	}
	ciphertext, nonce, err := d.Box.Seal(plaintext)
	if err != nil {
		return fmt.Errorf("secrets: seal: %w", err)
	}
	q := actionsdb.New()
	creator := pgtype.Int8{Int64: createdBy, Valid: createdBy != 0}
	switch {
	case scope.IsRepo():
		_, err = q.UpsertRepoSecret(ctx, d.Pool, actionsdb.UpsertRepoSecretParams{
			RepoID:          pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:            name,
			Ciphertext:      ciphertext,
			Nonce:           nonce,
			CreatedByUserID: creator,
		})
	case scope.IsOrg():
		_, err = q.UpsertOrgSecret(ctx, d.Pool, actionsdb.UpsertOrgSecretParams{
			OrgID:           pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:            name,
			Ciphertext:      ciphertext,
			Nonce:           nonce,
			CreatedByUserID: creator,
		})
	case scope.IsUser():
		_, err = q.UpsertUserSecret(ctx, d.Pool, actionsdb.UpsertUserSecretParams{
			UserID:          pgtype.Int8{Int64: scope.UserID, Valid: true},
			Name:            name,
			Ciphertext:      ciphertext,
			Nonce:           nonce,
			CreatedByUserID: creator,
		})
	case scope.IsEnvironment():
		_, err = q.UpsertEnvironmentSecret(ctx, d.Pool, actionsdb.UpsertEnvironmentSecretParams{
			EnvironmentID:   pgtype.Int8{Int64: scope.EnvironmentID, Valid: true},
			Name:            name,
			Ciphertext:      ciphertext,
			Nonce:           nonce,
			CreatedByUserID: creator,
		})
	}
	if err != nil {
		return fmt.Errorf("secrets: upsert: %w", err)
	}
	return nil
}

// Get returns the decrypted plaintext for the secret named in scope.
// Used only by the runner-side claim resolver (S41c-2) where the
// runner has authorization to receive secret values for its job's
// scope. **Never** call this from a web handler — the UI lists names
// only.
func (d Deps) Get(ctx context.Context, scope Scope, name string) ([]byte, error) {
	if !scope.IsRepo() && !scope.IsOrg() && !scope.IsUser() && !scope.IsEnvironment() {
		return nil, ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	q := actionsdb.New()
	var ct, nonce []byte
	var err error
	switch {
	case scope.IsRepo():
		row, qerr := q.GetRepoSecret(ctx, d.Pool, actionsdb.GetRepoSecretParams{
			RepoID: pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:   name,
		})
		err = qerr
		ct, nonce = row.Ciphertext, row.Nonce
	case scope.IsOrg():
		row, qerr := q.GetOrgSecret(ctx, d.Pool, actionsdb.GetOrgSecretParams{
			OrgID: pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:  name,
		})
		err = qerr
		ct, nonce = row.Ciphertext, row.Nonce
	case scope.IsUser():
		row, qerr := q.GetUserSecret(ctx, d.Pool, actionsdb.GetUserSecretParams{
			UserID: pgtype.Int8{Int64: scope.UserID, Valid: true},
			Name:   name,
		})
		err = qerr
		ct, nonce = row.Ciphertext, row.Nonce
	case scope.IsEnvironment():
		row, qerr := q.GetEnvironmentSecret(ctx, d.Pool, actionsdb.GetEnvironmentSecretParams{
			EnvironmentID: pgtype.Int8{Int64: scope.EnvironmentID, Valid: true},
			Name:          name,
		})
		err = qerr
		ct, nonce = row.Ciphertext, row.Nonce
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("secrets: get: %w", err)
	}
	plaintext, err := d.Box.Open(ct, nonce)
	if err != nil {
		return nil, fmt.Errorf("secrets: open: %w", err)
	}
	return plaintext, nil
}

// List returns the names + metadata for every secret in scope. No
// ciphertext, no plaintext — the public listing shape only. Names are
// sorted ascending for stable UI rendering.
func (d Deps) List(ctx context.Context, scope Scope) ([]Meta, error) {
	if !scope.IsRepo() && !scope.IsOrg() && !scope.IsUser() && !scope.IsEnvironment() {
		return nil, ErrInvalidScope
	}
	q := actionsdb.New()
	switch {
	case scope.IsRepo():
		rows, err := q.ListRepoSecrets(ctx, d.Pool, pgtype.Int8{Int64: scope.RepoID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("secrets: list: %w", err)
		}
		out := make([]Meta, len(rows))
		for i, r := range rows {
			out[i] = Meta{
				ID:              r.ID,
				Name:            string(r.Name),
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	case scope.IsOrg():
		rows, err := q.ListOrgSecrets(ctx, d.Pool, pgtype.Int8{Int64: scope.OrgID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("secrets: list: %w", err)
		}
		out := make([]Meta, len(rows))
		for i, r := range rows {
			out[i] = Meta{
				ID:              r.ID,
				Name:            string(r.Name),
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	case scope.IsUser():
		rows, err := q.ListUserSecrets(ctx, d.Pool, pgtype.Int8{Int64: scope.UserID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("secrets: list: %w", err)
		}
		out := make([]Meta, len(rows))
		for i, r := range rows {
			out[i] = Meta{
				ID:              r.ID,
				Name:            string(r.Name),
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	case scope.IsEnvironment():
		rows, err := q.ListEnvironmentSecrets(ctx, d.Pool, pgtype.Int8{Int64: scope.EnvironmentID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("secrets: list: %w", err)
		}
		out := make([]Meta, len(rows))
		for i, r := range rows {
			out[i] = Meta{
				ID:              r.ID,
				Name:            string(r.Name),
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	}
	return nil, ErrInvalidScope
}

// GetMeta returns metadata for a single secret without decrypting the
// ciphertext. The REST GET-by-name handler uses this instead of List +
// linear scan; same one round-trip cost as a List, but bounded to a
// single row.
//
// User scope is the only kind wired today — the repo + org REST GETs
// still scan their List output. They're the same shape and a future
// cleanup pass can mirror this. Audit (PRO-EXT_SR-08) only flagged
// the user-scope path.
func (d Deps) GetMeta(ctx context.Context, scope Scope, name string) (Meta, error) {
	if !scope.IsUser() {
		return Meta{}, ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return Meta{}, err
	}
	row, err := actionsdb.New().GetUserSecretMeta(ctx, d.Pool, actionsdb.GetUserSecretMetaParams{
		UserID: pgtype.Int8{Int64: scope.UserID, Valid: true},
		Name:   name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Meta{}, ErrNotFound
		}
		return Meta{}, fmt.Errorf("secrets: get meta: %w", err)
	}
	return Meta{
		ID:              row.ID,
		Name:            row.Name,
		CreatedByUserID: int64ValueOrZero(row.CreatedByUserID),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}, nil
}

// Delete removes a secret. Returns ErrNotFound when no row matched
// the (scope, name) tuple — I26 (audit) pre-fix the SQL was a bare
// DELETE WHERE which silently affected zero rows; the handler then
// returned 204 ("success") for a delete that did nothing. The DELETE
// queries now use sqlc :execrows so we can fan out "0 rows affected"
// to ErrNotFound and the handler can map that to HTTP 404 (gh-compat).
func (d Deps) Delete(ctx context.Context, scope Scope, name string) error {
	if !scope.IsRepo() && !scope.IsOrg() && !scope.IsUser() && !scope.IsEnvironment() {
		return ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return err
	}
	q := actionsdb.New()
	var (
		rows int64
		err  error
	)
	switch {
	case scope.IsRepo():
		rows, err = q.DeleteRepoSecret(ctx, d.Pool, actionsdb.DeleteRepoSecretParams{
			RepoID: pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:   name,
		})
	case scope.IsOrg():
		rows, err = q.DeleteOrgSecret(ctx, d.Pool, actionsdb.DeleteOrgSecretParams{
			OrgID: pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:  name,
		})
	case scope.IsUser():
		rows, err = q.DeleteUserSecret(ctx, d.Pool, actionsdb.DeleteUserSecretParams{
			UserID: pgtype.Int8{Int64: scope.UserID, Valid: true},
			Name:   name,
		})
	case scope.IsEnvironment():
		return q.DeleteEnvironmentSecret(ctx, d.Pool, actionsdb.DeleteEnvironmentSecretParams{
			EnvironmentID: pgtype.Int8{Int64: scope.EnvironmentID, Valid: true},
			Name:          name,
		})
	default:
		return ErrInvalidScope
	}
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func int64ValueOrZero(p pgtype.Int8) int64 {
	if p.Valid {
		return p.Int64
	}
	return 0
}
