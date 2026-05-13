// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
)

// AddMember inserts an (org, user) pair with the supplied role.
// Idempotent on the pair: a re-add for an existing member is a no-op
// at the DB layer (matches the sqlc query's ON CONFLICT DO NOTHING).
// Caller is responsible for the policy check that the actor is allowed
// to manage members.
func AddMember(ctx context.Context, deps Deps, orgID, userID, invitedByUserID int64, role string) error {
	if role == "" {
		role = "member"
	}
	r, err := parseRole(role)
	if err != nil {
		return err
	}
	if r == orgsdb.OrgRoleOwner {
		check, err := entitlements.CheckOrgOwnerPrivateCollaboration(ctx, entitlements.Deps{Pool: deps.Pool}, orgID, userID)
		if err != nil {
			return err
		}
		if err := check.Err(); err != nil {
			return err
		}
	}
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	q := orgsdb.New()
	if err := q.AddOrgMember(ctx, tx, orgsdb.AddOrgMemberParams{
		OrgID:           orgID,
		UserID:          userID,
		Role:            r,
		InvitedByUserID: pgtype.Int8{Int64: invitedByUserID, Valid: invitedByUserID != 0},
	}); err != nil {
		return err
	}
	if err := enqueueBillingSeatSync(ctx, tx, deps, orgID); err != nil {
		return fmt.Errorf("enqueue billing seat sync: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// ChangeRole updates a member's role with last-owner protection: the
// only owner cannot be demoted (refuse with ErrLastOwner). Caller has
// already verified the actor's policy.
func ChangeRole(ctx context.Context, deps Deps, orgID, userID int64, role string) error {
	r, err := parseRole(role)
	if err != nil {
		return err
	}
	q := orgsdb.New()
	current, err := q.GetOrgMember(ctx, deps.Pool, orgsdb.GetOrgMemberParams{
		OrgID: orgID, UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotAMember
		}
		return err
	}
	if current.Role == orgsdb.OrgRoleOwner && r != orgsdb.OrgRoleOwner {
		// Demoting an owner: refuse if they're the last one.
		count, err := q.CountOrgOwners(ctx, deps.Pool, orgID)
		if err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastOwner
		}
	}
	if current.Role != orgsdb.OrgRoleOwner && r == orgsdb.OrgRoleOwner {
		check, err := entitlements.CheckOrgOwnerPrivateCollaboration(ctx, entitlements.Deps{Pool: deps.Pool}, orgID, userID)
		if err != nil {
			return err
		}
		if err := check.Err(); err != nil {
			return err
		}
	}
	return q.ChangeOrgMemberRole(ctx, deps.Pool, orgsdb.ChangeOrgMemberRoleParams{
		OrgID: orgID, UserID: userID, Role: r,
	})
}

// RemoveMember deletes the (org, user) row. Last-owner protection
// applies — we refuse to drop the only owner. Removing oneself is
// fine when there are ≥2 owners.
func RemoveMember(ctx context.Context, deps Deps, orgID, userID int64) error {
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	q := orgsdb.New()
	current, err := q.GetOrgMember(ctx, tx, orgsdb.GetOrgMemberParams{
		OrgID: orgID, UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotAMember
		}
		return err
	}
	if current.Role == orgsdb.OrgRoleOwner {
		count, err := q.CountOrgOwners(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastOwner
		}
	}
	if err := q.RemoveOrgMember(ctx, tx, orgsdb.RemoveOrgMemberParams{
		OrgID: orgID, UserID: userID,
	}); err != nil {
		return err
	}
	if err := enqueueBillingSeatSync(ctx, tx, deps, orgID); err != nil {
		return fmt.Errorf("enqueue billing seat sync: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// IsMember reports whether the user is a member of the org. Used by
// policy + the org-owner repo-create gate.
func IsMember(ctx context.Context, deps Deps, orgID, userID int64) (bool, error) {
	_, err := orgsdb.New().GetOrgMember(ctx, deps.Pool, orgsdb.GetOrgMemberParams{
		OrgID: orgID, UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsOwner reports whether the user is an owner of the org.
func IsOwner(ctx context.Context, deps Deps, orgID, userID int64) (bool, error) {
	row, err := orgsdb.New().GetOrgMember(ctx, deps.Pool, orgsdb.GetOrgMemberParams{
		OrgID: orgID, UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return row.Role == orgsdb.OrgRoleOwner, nil
}

// parseRole returns the typed enum value, or an error for unknown
// strings (defends against a hand-crafted POST body).
func parseRole(s string) (orgsdb.OrgRole, error) {
	switch s {
	case "owner":
		return orgsdb.OrgRoleOwner, nil
	case "member":
		return orgsdb.OrgRoleMember, nil
	default:
		return "", fmt.Errorf("orgs: invalid role %q", s)
	}
}
