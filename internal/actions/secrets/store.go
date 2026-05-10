// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets owns the orchestrator over `workflow_secrets`. The
// table holds AEAD-encrypted blobs (ChaCha20Poly1305 via
// internal/auth/secretbox); plaintext never lives in postgres.
//
// Two scopes share the table:
//
//   - **Repo secrets**: visible only to workflows running in that repo.
//   - **Org secrets**: visible to workflows running in any of the org's
//     repos. Repo-scoped secrets shadow org secrets with the same name
//     (resolution order is repo → org).
//
// The XOR is enforced by a CHECK on the table; the typed Scope here
// is the in-Go mirror — exactly one of RepoID / OrgID is set. Callers
// always go through Scope helpers (RepoScope, OrgScope) so the
// XOR isn't a struct-literal trap.
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
// Construct via RepoScope or OrgScope; RepoID and OrgID are mutually
// exclusive (the table CHECK constraint enforces this server-side).
type Scope struct {
	RepoID int64
	OrgID  int64
}

// RepoScope returns a repo-scoped Scope. Repo secrets are visible only
// to workflows running in that repo.
func RepoScope(id int64) Scope { return Scope{RepoID: id} }

// OrgScope returns an org-scoped Scope. Org secrets are visible to
// workflows running in any repo owned by the org.
func OrgScope(id int64) Scope { return Scope{OrgID: id} }

// IsRepo reports whether the scope addresses a repo. Mutex with IsOrg.
func (s Scope) IsRepo() bool { return s.RepoID != 0 && s.OrgID == 0 }

// IsOrg reports whether the scope addresses an org. Mutex with IsRepo.
func (s Scope) IsOrg() bool { return s.OrgID != 0 && s.RepoID == 0 }

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
	// ErrInvalidScope: zero-or-both Scope fields. Programmer error.
	ErrInvalidScope = errors.New("secrets: scope must address exactly one of RepoID or OrgID")
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
	if !scope.IsRepo() && !scope.IsOrg() {
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
	if !scope.IsRepo() && !scope.IsOrg() {
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
	if !scope.IsRepo() && !scope.IsOrg() {
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
	}
	return nil, ErrInvalidScope
}

// Delete removes a secret. Returns ErrNotFound when the row didn't
// exist; idempotent at the SQL layer (DELETE WHERE).
func (d Deps) Delete(ctx context.Context, scope Scope, name string) error {
	if !scope.IsRepo() && !scope.IsOrg() {
		return ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return err
	}
	q := actionsdb.New()
	switch {
	case scope.IsRepo():
		return q.DeleteRepoSecret(ctx, d.Pool, actionsdb.DeleteRepoSecretParams{
			RepoID: pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:   name,
		})
	case scope.IsOrg():
		return q.DeleteOrgSecret(ctx, d.Pool, actionsdb.DeleteOrgSecretParams{
			OrgID: pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:  name,
		})
	}
	return ErrInvalidScope
}

func int64ValueOrZero(p pgtype.Int8) int64 {
	if p.Valid {
		return p.Int64
	}
	return 0
}
