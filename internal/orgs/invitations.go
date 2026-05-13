// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// InviteParams describes a new invitation. Exactly one of
// TargetUsername / TargetEmail must be non-empty; the resolver below
// converts a username to a user_id via the users table.
type InviteParams struct {
	OrgID           int64
	InvitedByUserID int64
	TargetUsername  string
	TargetEmail     string
	Role            string // "owner" | "member"
}

// InviteResult bundles the freshly-created invitation row + the
// plaintext token. Callers typically render the token into a URL and
// drop it into an email; the token IS the credential — never persist
// it after the email send.
type InviteResult struct {
	Invitation orgsdb.OrgInvitation
	Token      string
}

// Invite creates a pending invitation. Resolves a username to a
// user_id when provided; otherwise treats the input as an email
// address (recipients without an account claim it on signup).
func Invite(ctx context.Context, deps Deps, p InviteParams) (InviteResult, error) {
	if p.OrgID == 0 || p.InvitedByUserID == 0 {
		return InviteResult{}, errors.New("orgs: OrgID + InvitedByUserID required")
	}
	if (p.TargetUsername == "" && p.TargetEmail == "") ||
		(p.TargetUsername != "" && p.TargetEmail != "") {
		return InviteResult{}, ErrInvalidInvitationKind
	}
	role, err := parseRole(p.Role)
	if err != nil {
		return InviteResult{}, err
	}

	q := orgsdb.New()

	var (
		targetUserID pgtype.Int8
		targetEmail  pgtype.Text
	)
	if p.TargetUsername != "" {
		uname := strings.ToLower(strings.TrimSpace(p.TargetUsername))
		u, err := usersdb.New().GetUserByUsername(ctx, deps.Pool, uname)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return InviteResult{}, ErrUserNotFound
			}
			return InviteResult{}, err
		}
		// Already a member? short-circuit.
		if _, err := q.GetOrgMember(ctx, deps.Pool, orgsdb.GetOrgMemberParams{
			OrgID: p.OrgID, UserID: u.ID,
		}); err == nil {
			return InviteResult{}, ErrAlreadyMember
		}
		targetUserID = pgtype.Int8{Int64: u.ID, Valid: true}
	} else {
		email := strings.ToLower(strings.TrimSpace(p.TargetEmail))
		if email == "" {
			return InviteResult{}, ErrInvalidInvitationKind
		}
		targetEmail = pgtype.Text{String: email, Valid: true}
	}

	// Idempotency: if a pending invite for this target already exists,
	// surface it rather than minting a fresh one (avoids token churn
	// + accidental spam from re-clicked Invite buttons).
	if _, err := q.GetExistingPendingInvitation(ctx, deps.Pool, orgsdb.GetExistingPendingInvitationParams{
		OrgID:        p.OrgID,
		TargetUserID: targetUserID,
		TargetEmail:  emailToCitext(targetEmail),
	}); err == nil {
		return InviteResult{}, ErrInvitationDuplicate
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return InviteResult{}, err
	}
	if role == orgsdb.OrgRoleOwner {
		var check entitlements.PrivateCollaborationCheck
		if targetUserID.Valid {
			check, err = entitlements.CheckOrgOwnerPrivateCollaboration(ctx, entitlements.Deps{Pool: deps.Pool}, p.OrgID, targetUserID.Int64)
		} else {
			check, err = entitlements.CheckPrivateInvitationSlot(ctx, entitlements.Deps{Pool: deps.Pool}, p.OrgID)
		}
		if err != nil {
			return InviteResult{}, err
		}
		if err := check.Err(); err != nil {
			return InviteResult{}, err
		}
	}

	tokEnc, tokHash, err := token.New()
	if err != nil {
		return InviteResult{}, fmt.Errorf("invite token: %w", err)
	}
	row, err := q.CreateOrgInvitation(ctx, deps.Pool, orgsdb.CreateOrgInvitationParams{
		OrgID:           p.OrgID,
		InvitedByUserID: pgtype.Int8{Int64: p.InvitedByUserID, Valid: true},
		TargetUserID:    targetUserID,
		TargetEmail:     emailToCitext(targetEmail),
		Role:            role,
		TokenHash:       tokHash,
		ExpiresAt:       pgtype.Timestamptz{Time: time.Now().Add(7 * 24 * time.Hour), Valid: true},
	})
	if err != nil {
		return InviteResult{}, fmt.Errorf("create invitation: %w", err)
	}

	// Best-effort email — failures don't break the invitation row.
	go h.tryEmailInvite(deps, row, tokEnc) //nolint:gocritic // closure is fine

	return InviteResult{Invitation: row, Token: tokEnc}, nil
}

// h is a tiny package-private helper struct so the closure in Invite
// can pin the email-side code without dragging extra fields into
// every call site. Keeps Invite signature focused.
var h struct {
	tryEmailInvite func(deps Deps, row orgsdb.OrgInvitation, tokEnc string)
}

func init() {
	h.tryEmailInvite = func(deps Deps, row orgsdb.OrgInvitation, tokEnc string) {
		if deps.EmailSender == nil {
			return
		}
		var to string
		if row.TargetEmail.Valid {
			to = string(row.TargetEmail.String)
		} else if row.TargetUserID.Valid {
			u, err := usersdb.New().GetUserByID(context.Background(), deps.Pool, row.TargetUserID.Int64)
			if err != nil || !u.PrimaryEmailID.Valid {
				return
			}
			em, err := usersdb.New().GetUserEmailByID(context.Background(), deps.Pool, u.PrimaryEmailID.Int64)
			if err != nil || !em.Verified {
				return
			}
			to = string(em.Email)
		}
		if to == "" {
			return
		}
		url := strings.TrimRight(deps.BaseURL, "/") + "/invitations/" + tokEnc
		text := "You've been invited to join an organization on " + deps.SiteName + ".\n\n" +
			"Accept or decline: " + url + "\n\n" +
			"This link expires in 7 days.\n"
		_ = deps.EmailSender.Send(context.Background(), email.Message{
			From:    deps.EmailFrom,
			To:      to,
			Subject: "[" + deps.SiteName + "] You've been invited to an organization",
			Text:    text,
			HTML:    "<p>You've been invited to join an organization on " + deps.SiteName + ".</p><p><a href=\"" + url + "\">Accept or decline</a></p><p>This link expires in 7 days.</p>",
		})
	}
}

// AcceptInvitation marks an invitation accepted and adds the target
// as a member. The acceptor is the currently-logged-in user; for
// email-based invites the acceptor's verified email must match the
// invite's target_email.
func AcceptInvitation(ctx context.Context, deps Deps, inv orgsdb.OrgInvitation, acceptorUserID int64) error {
	if err := validatePending(inv); err != nil {
		return err
	}
	// Email-target invites: claim only when the acceptor owns the
	// email (verified primary). Username-target invites: only the
	// matching user can accept.
	if inv.TargetUserID.Valid && inv.TargetUserID.Int64 != acceptorUserID {
		return ErrUnauthorizedAcceptor
	}
	if inv.TargetEmail.Valid {
		ok, err := userOwnsVerifiedEmail(ctx, deps, acceptorUserID, string(inv.TargetEmail.String))
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnauthorizedAcceptor
		}
	}
	if inv.Role == orgsdb.OrgRoleOwner {
		check, err := entitlements.CheckOrgOwnerPrivateCollaboration(ctx, entitlements.Deps{Pool: deps.Pool}, inv.OrgID, acceptorUserID)
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
		OrgID:           inv.OrgID,
		UserID:          acceptorUserID,
		Role:            inv.Role,
		InvitedByUserID: inv.InvitedByUserID,
	}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	if err := q.AcceptOrgInvitation(ctx, tx, inv.ID); err != nil {
		return fmt.Errorf("mark accepted: %w", err)
	}
	if err := enqueueBillingSeatSync(ctx, tx, deps, inv.OrgID); err != nil {
		return fmt.Errorf("enqueue billing seat sync: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// DeclineInvitation marks an invitation declined. Same target-binding
// rules as AcceptInvitation apply.
func DeclineInvitation(ctx context.Context, deps Deps, inv orgsdb.OrgInvitation, declinerUserID int64) error {
	if err := validatePending(inv); err != nil {
		return err
	}
	if inv.TargetUserID.Valid && inv.TargetUserID.Int64 != declinerUserID {
		return ErrUnauthorizedAcceptor
	}
	if inv.TargetEmail.Valid {
		ok, err := userOwnsVerifiedEmail(ctx, deps, declinerUserID, string(inv.TargetEmail.String))
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnauthorizedAcceptor
		}
	}
	return orgsdb.New().DeclineOrgInvitation(ctx, deps.Pool, inv.ID)
}

// CancelInvitation marks an invitation canceled. Caller is the org
// owner / inviter; policy is checked before this is invoked.
func CancelInvitation(ctx context.Context, deps Deps, inv orgsdb.OrgInvitation) error {
	if err := validatePending(inv); err != nil {
		return err
	}
	return orgsdb.New().CancelOrgInvitation(ctx, deps.Pool, inv.ID)
}

// LookupInvitationByToken fetches the invitation matching the
// supplied bearer token. Returns ErrInvitationNotFound when the token
// doesn't match a row.
func LookupInvitationByToken(ctx context.Context, deps Deps, encodedToken string) (orgsdb.OrgInvitation, error) {
	hash, err := token.HashOf(encodedToken)
	if err != nil {
		return orgsdb.OrgInvitation{}, ErrInvitationNotFound
	}
	row, err := orgsdb.New().GetOrgInvitationByTokenHash(ctx, deps.Pool, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return orgsdb.OrgInvitation{}, ErrInvitationNotFound
		}
		return orgsdb.OrgInvitation{}, err
	}
	return row, nil
}

// ─── helpers ───────────────────────────────────────────────────────

// ErrUnauthorizedAcceptor is returned when an acceptor doesn't own
// the email the invite was issued to (or doesn't match the invited
// user).
var ErrUnauthorizedAcceptor = errors.New("orgs: invitation does not match this user")

func validatePending(inv orgsdb.OrgInvitation) error {
	if inv.AcceptedAt.Valid || inv.DeclinedAt.Valid || inv.CanceledAt.Valid {
		return ErrInvitationConsumed
	}
	if !inv.ExpiresAt.Valid || inv.ExpiresAt.Time.Before(time.Now()) {
		return ErrInvitationExpired
	}
	return nil
}

func userOwnsVerifiedEmail(ctx context.Context, deps Deps, userID int64, email string) (bool, error) {
	rows, err := usersdb.New().ListUserEmailsForUser(ctx, deps.Pool, userID)
	if err != nil {
		return false, err
	}
	low := strings.ToLower(email)
	for _, e := range rows {
		if strings.ToLower(string(e.Email)) == low && e.Verified {
			return true, nil
		}
	}
	return false, nil
}

// emailToCitext is a tiny shim because the sqlc-generated email
// column type for `target_email` is a non-pointer pgtype.Text-flavored
// citext. Pass-through when valid; zero value otherwise.
func emailToCitext(p pgtype.Text) pgtype.Text {
	if !p.Valid {
		return pgtype.Text{Valid: false}
	}
	return p
}
