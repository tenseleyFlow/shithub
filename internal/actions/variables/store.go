// SPDX-License-Identifier: AGPL-3.0-or-later

// Package variables owns the orchestrator over `actions_variables`.
// Variables are non-secret, plaintext configuration surfaced to workflows
// through the `${{ vars.NAME }}` namespace. They share the same repo/org
// scoping rules as actions secrets, but List/Get intentionally return values
// because these rows are not sensitive and are operator-visible in settings UI.
package variables

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

// MaxValueChars mirrors the actions_variables_value_length CHECK constraint.
const MaxValueChars = 4096

// Deps wires the store against runtime infra.
type Deps struct {
	Pool *pgxpool.Pool
}

// Scope identifies which `actions_variables` row family a call targets.
// Construct via RepoScope or OrgScope; RepoID and OrgID are mutually
// exclusive (the table CHECK constraint enforces this server-side).
type Scope struct {
	RepoID int64
	OrgID  int64
}

// RepoScope returns a repo-scoped Scope. Repo variables are visible only
// to workflows running in that repo.
func RepoScope(id int64) Scope { return Scope{RepoID: id} }

// OrgScope returns an org-scoped Scope. Org variables are visible to
// workflows running in any repo owned by the org.
func OrgScope(id int64) Scope { return Scope{OrgID: id} }

// IsRepo reports whether the scope addresses a repo. Mutex with IsOrg.
func (s Scope) IsRepo() bool { return s.RepoID != 0 && s.OrgID == 0 }

// IsOrg reports whether the scope addresses an org. Mutex with IsRepo.
func (s Scope) IsOrg() bool { return s.OrgID != 0 && s.RepoID == 0 }

// Variable is the public listing/lookup shape. Values are intentionally
// present because variables are not secrets; use the secrets package for
// sensitive data that must never render in the web UI.
type Variable struct {
	ID              int64
	Name            string
	Value           string
	CreatedByUserID int64 // 0 when null
	CreatedAt       pgtype.Timestamptz
	UpdatedAt       pgtype.Timestamptz
}

// Errors surfaced by the store. Callers (web handlers, runner API)
// map these to HTTP status codes.
var (
	// ErrInvalidScope: zero-or-both Scope fields. Programmer error.
	ErrInvalidScope = errors.New("variables: scope must address exactly one of RepoID or OrgID")
	// ErrInvalidName: name doesn't match the regex. User-recoverable.
	ErrInvalidName = errors.New("variables: name must match ^[A-Za-z_][A-Za-z0-9_]*$ and be 1..100 chars")
	// ErrValueTooLong: values are plaintext config, capped to keep UI and
	// expression-eval memory bounded.
	ErrValueTooLong = errors.New("variables: value must be 4096 chars or fewer")
	// ErrNotFound: no row with the given name in this scope.
	ErrNotFound = errors.New("variables: not found")
)

// nameRe mirrors the actions_variables_name_format CHECK in migration 0049.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateName(name string) error {
	if len(name) < 1 || len(name) > 100 {
		return ErrInvalidName
	}
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

func validateValue(value string) error {
	if utf8.RuneCountInString(value) > MaxValueChars {
		return ErrValueTooLong
	}
	return nil
}

// Set creates or updates a variable in scope. Empty string is valid:
// `${{ vars.MISSING }}` already resolves to empty string, and operators may
// intentionally pin the same value explicitly.
func (d Deps) Set(ctx context.Context, scope Scope, name, value string, createdBy int64) error {
	if !scope.IsRepo() && !scope.IsOrg() {
		return ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	q := actionsdb.New()
	creator := pgtype.Int8{Int64: createdBy, Valid: createdBy != 0}
	switch {
	case scope.IsRepo():
		_, err := q.UpsertRepoVariable(ctx, d.Pool, actionsdb.UpsertRepoVariableParams{
			RepoID:          pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:            name,
			Value:           value,
			CreatedByUserID: creator,
		})
		if err != nil {
			return fmt.Errorf("variables: upsert repo: %w", err)
		}
	case scope.IsOrg():
		_, err := q.UpsertOrgVariable(ctx, d.Pool, actionsdb.UpsertOrgVariableParams{
			OrgID:           pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:            name,
			Value:           value,
			CreatedByUserID: creator,
		})
		if err != nil {
			return fmt.Errorf("variables: upsert org: %w", err)
		}
	}
	return nil
}

// Get returns one plaintext variable by name.
func (d Deps) Get(ctx context.Context, scope Scope, name string) (Variable, error) {
	if !scope.IsRepo() && !scope.IsOrg() {
		return Variable{}, ErrInvalidScope
	}
	if err := validateName(name); err != nil {
		return Variable{}, err
	}
	q := actionsdb.New()
	switch {
	case scope.IsRepo():
		row, err := q.GetRepoVariable(ctx, d.Pool, actionsdb.GetRepoVariableParams{
			RepoID: pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:   name,
		})
		if err != nil {
			return Variable{}, mapGetErr(err)
		}
		return Variable{
			ID:              row.ID,
			Name:            row.Name,
			Value:           row.Value,
			CreatedByUserID: int64ValueOrZero(row.CreatedByUserID),
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		}, nil
	case scope.IsOrg():
		row, err := q.GetOrgVariable(ctx, d.Pool, actionsdb.GetOrgVariableParams{
			OrgID: pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:  name,
		})
		if err != nil {
			return Variable{}, mapGetErr(err)
		}
		return Variable{
			ID:              row.ID,
			Name:            row.Name,
			Value:           row.Value,
			CreatedByUserID: int64ValueOrZero(row.CreatedByUserID),
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		}, nil
	}
	return Variable{}, ErrInvalidScope
}

// List returns every variable in scope, sorted ascending for stable UI
// rendering.
func (d Deps) List(ctx context.Context, scope Scope) ([]Variable, error) {
	if !scope.IsRepo() && !scope.IsOrg() {
		return nil, ErrInvalidScope
	}
	q := actionsdb.New()
	switch {
	case scope.IsRepo():
		rows, err := q.ListRepoVariables(ctx, d.Pool, pgtype.Int8{Int64: scope.RepoID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("variables: list repo: %w", err)
		}
		out := make([]Variable, len(rows))
		for i, r := range rows {
			out[i] = Variable{
				ID:              r.ID,
				Name:            r.Name,
				Value:           r.Value,
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	case scope.IsOrg():
		rows, err := q.ListOrgVariables(ctx, d.Pool, pgtype.Int8{Int64: scope.OrgID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("variables: list org: %w", err)
		}
		out := make([]Variable, len(rows))
		for i, r := range rows {
			out[i] = Variable{
				ID:              r.ID,
				Name:            r.Name,
				Value:           r.Value,
				CreatedByUserID: int64ValueOrZero(r.CreatedByUserID),
				CreatedAt:       r.CreatedAt,
				UpdatedAt:       r.UpdatedAt,
			}
		}
		return out, nil
	}
	return nil, ErrInvalidScope
}

// Delete removes a variable. Missing rows are treated as success so callers
// can use Delete for cleanup without a read-before-write race.
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
		return q.DeleteRepoVariable(ctx, d.Pool, actionsdb.DeleteRepoVariableParams{
			RepoID: pgtype.Int8{Int64: scope.RepoID, Valid: true},
			Name:   name,
		})
	case scope.IsOrg():
		return q.DeleteOrgVariable(ctx, d.Pool, actionsdb.DeleteOrgVariableParams{
			OrgID: pgtype.Int8{Int64: scope.OrgID, Valid: true},
			Name:  name,
		})
	}
	return ErrInvalidScope
}

func mapGetErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("variables: get: %w", err)
}

func int64ValueOrZero(p pgtype.Int8) int64 {
	if p.Valid {
		return p.Int64
	}
	return 0
}
