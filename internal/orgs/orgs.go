// SPDX-License-Identifier: AGPL-3.0-or-later

// Package orgs owns the organization domain: create, members,
// invitations, suspend/restore, soft-delete, and the principals
// resolver that unifies /{slug} routing across users + orgs.
//
// The package is the single entry point for the rest of the runtime —
// web handlers, the policy engine, and the worker pool import here
// rather than poking the sqlc surface directly.
package orgs

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
)

// Deps wires this package against the runtime. Pool is required;
// EmailSender is optional — when nil, invitation + role-change
// notifications are skipped (matches the "headless test" pattern
// the rest of the auth surface uses).
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Audit  *audit.Recorder
	// Email side: only used by the invitation flow today; the
	// org-suspend / org-deletion notification kinds are deferred
	// to S29's lifecycle email work.
	EmailSender email.Sender
	EmailFrom   string
	SiteName    string
	BaseURL     string
}

// Errors surfaced by the orchestrator. Web handlers map these to
// status codes + friendly messages.
var (
	ErrEmptySlug             = errors.New("orgs: slug is required")
	ErrSlugTooLong           = errors.New("orgs: slug too long (max 39)")
	ErrSlugInvalid           = errors.New("orgs: slug must be lowercase letters, digits, and hyphens")
	ErrSlugReserved          = errors.New("orgs: slug is reserved")
	ErrSlugTaken             = errors.New("orgs: slug is already in use")
	ErrOrgNotFound           = errors.New("orgs: org not found")
	ErrUserNotFound          = errors.New("orgs: user not found")
	ErrNotAMember            = errors.New("orgs: user is not a member of this org")
	ErrAlreadyMember         = errors.New("orgs: user is already a member of this org")
	ErrLastOwner             = errors.New("orgs: cannot remove or demote the only owner; transfer ownership first")
	ErrInvitationNotFound    = errors.New("orgs: invitation not found")
	ErrInvitationExpired     = errors.New("orgs: invitation has expired")
	ErrInvitationConsumed    = errors.New("orgs: invitation already accepted, declined, or canceled")
	ErrInvitationDuplicate   = errors.New("orgs: a pending invitation for this target already exists")
	ErrInvalidInvitationKind = errors.New("orgs: invitation must target either a user or an email")
	ErrSuspended             = errors.New("orgs: org is suspended")
	ErrDeleted               = errors.New("orgs: org is soft-deleted")
)

// InvitationLifetime is how long a fresh invitation is honored before
// the recipient must request a new one. 7 days matches the spec.
const InvitationLifetime = 7 * 24 * 60 * 60 // seconds; converted to time.Duration at call sites
