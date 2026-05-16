// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

// PrincipalState is the unified projection of a subject's billing
// state that the webhook handler and Principal-shaped service
// callers consume. It carries only the fields used for routing /
// transition decisions; full org or user state stays accessible
// via GetOrgBillingState / GetUserBillingState when the caller
// already knows the kind and wants table-specific columns.
type PrincipalState struct {
	Principal            Principal
	Plan                 string // user_plan or org_plan, narrowed to string for kind-agnostic logging
	SubscriptionStatus   SubscriptionStatus
	StripeCustomerID     string
	StripeSubscriptionID string
	CancelAtPeriodEnd    bool
	LockedAt             time.Time
}

// ErrPrincipalNotFound signals that no row matched a
// Stripe-customer-id or subscription-id lookup on either table.
// Callers translate to a user-visible error or fall through to the
// metadata-resolution path.
var ErrPrincipalNotFound = errors.New("billing: principal not found")

// GetStateForPrincipal returns the unified state for `p`. Branches
// to org or user sqlc query based on p.Kind. Surfaces
// ErrInvalidPrincipal for malformed input; pgx.ErrNoRows for
// missing rows (caller's responsibility to handle).
func GetStateForPrincipal(ctx context.Context, deps Deps, p Principal) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		state, err := q.GetOrgBillingState(ctx, deps.Pool, p.ID)
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrg(state), nil
	case SubjectKindUser:
		state, err := q.GetUserBillingState(ctx, deps.Pool, p.ID)
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUser(state), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// ResolvePrincipalByStripeCustomer searches both billing-state
// tables for `customerID`. Stripe customer-ids are globally unique
// per Stripe account, so at most one row matches; we check user
// table first (newer, smaller during launch) then org as a small
// optimization. Cross-table duplicate is impossible by the
// unique-index design; if one ever appears, this returns the first
// hit and a defensive caller in the webhook handler should refuse
// the apply with a loud log.
func ResolvePrincipalByStripeCustomer(ctx context.Context, deps Deps, customerID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return PrincipalState{}, ErrStripeCustomerID
	}
	q := billingdb.New()
	if user, err := q.GetUserBillingStateByStripeCustomer(ctx, deps.Pool, pgText(customerID)); err == nil {
		return principalStateFromUser(user), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return PrincipalState{}, err
	}
	if org, err := q.GetOrgBillingStateByStripeCustomer(ctx, deps.Pool, pgText(customerID)); err == nil {
		return principalStateFromOrg(org), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return PrincipalState{}, err
	}
	return PrincipalState{}, ErrPrincipalNotFound
}

// ResolvePrincipalByStripeSubscription is the subscription-id
// counterpart. Same dual-table search; same uniqueness guarantee.
func ResolvePrincipalByStripeSubscription(ctx context.Context, deps Deps, subID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	subID = strings.TrimSpace(subID)
	if subID == "" {
		return PrincipalState{}, ErrStripeSubscriptionID
	}
	q := billingdb.New()
	if user, err := q.GetUserBillingStateByStripeSubscription(ctx, deps.Pool, pgText(subID)); err == nil {
		return principalStateFromUser(user), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return PrincipalState{}, err
	}
	if org, err := q.GetOrgBillingStateByStripeSubscription(ctx, deps.Pool, pgText(subID)); err == nil {
		return principalStateFromOrg(org), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return PrincipalState{}, err
	}
	return PrincipalState{}, ErrPrincipalNotFound
}

// SetStripeCustomerForPrincipal binds a Stripe customer id to either
// the org or user billing state. The org-shaped SetStripeCustomer
// stays as a thin wrapper for callers that pre-date PRO04.
func SetStripeCustomerForPrincipal(ctx context.Context, deps Deps, p Principal, customerID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return PrincipalState{}, ErrStripeCustomerID
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		state, err := q.SetStripeCustomer(ctx, deps.Pool, billingdb.SetStripeCustomerParams{
			OrgID:            p.ID,
			StripeCustomerID: pgText(customerID),
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrg(state), nil
	case SubjectKindUser:
		state, err := q.SetUserStripeCustomer(ctx, deps.Pool, billingdb.SetUserStripeCustomerParams{
			UserID:           p.ID,
			StripeCustomerID: pgText(customerID),
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUser(state), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// PrincipalSubscriptionSnapshot is the kind-agnostic snapshot
// passed to ApplySubscriptionSnapshotForPrincipal. The webhook
// handler builds this from the resolved Principal + Stripe event;
// the kind-specific plan (Pro for user, Team for org) is set by
// the caller before passing in.
type PrincipalSubscriptionSnapshot struct {
	Principal                Principal
	Status                   SubscriptionStatus
	StripeSubscriptionID     string
	StripeSubscriptionItemID string
	LicensedSeats            int
	CurrentPeriodStart       time.Time
	CurrentPeriodEnd         time.Time
	CancelAtPeriodEnd        bool
	TrialEnd                 time.Time
	CanceledAt               time.Time
	LastWebhookEventID       string
}

// ApplySubscriptionSnapshotForPrincipal routes the snapshot to
// either the org or user sqlc apply query. The plan it writes is
// `team` for org kind, `pro` for user kind — there is no third
// option in PRO04 (Enterprise stays contact-sales).
func ApplySubscriptionSnapshotForPrincipal(ctx context.Context, deps Deps, snap PrincipalSubscriptionSnapshot) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := snap.Principal.Validate(); err != nil {
		return PrincipalState{}, err
	}
	if !validStatus(snap.Status) {
		return PrincipalState{}, fmt.Errorf("%w: %q", ErrInvalidStatus, snap.Status)
	}
	if snap.LicensedSeats < 0 {
		return PrincipalState{}, ErrInvalidSeatCount
	}
	q := billingdb.New()
	switch snap.Principal.Kind {
	case SubjectKindOrg:
		row, err := q.ApplySubscriptionSnapshot(ctx, deps.Pool, billingdb.ApplySubscriptionSnapshotParams{
			OrgID:                    snap.Principal.ID,
			Plan:                     billingdb.OrgPlanTeam,
			SubscriptionStatus:       snap.Status,
			StripeSubscriptionID:     pgText(snap.StripeSubscriptionID),
			StripeSubscriptionItemID: pgText(snap.StripeSubscriptionItemID),
			LicensedSeats:            int32(snap.LicensedSeats),
			CurrentPeriodStart:       pgTime(snap.CurrentPeriodStart),
			CurrentPeriodEnd:         pgTime(snap.CurrentPeriodEnd),
			CancelAtPeriodEnd:        snap.CancelAtPeriodEnd,
			TrialEnd:                 pgTime(snap.TrialEnd),
			CanceledAt:               pgTime(snap.CanceledAt),
			LastWebhookEventID:       strings.TrimSpace(snap.LastWebhookEventID),
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrgApply(row), nil
	case SubjectKindUser:
		row, err := q.ApplyUserSubscriptionSnapshot(ctx, deps.Pool, billingdb.ApplyUserSubscriptionSnapshotParams{
			UserID:                   snap.Principal.ID,
			Plan:                     billingdb.UserPlanPro,
			SubscriptionStatus:       snap.Status,
			StripeSubscriptionID:     pgText(snap.StripeSubscriptionID),
			StripeSubscriptionItemID: pgText(snap.StripeSubscriptionItemID),
			CurrentPeriodStart:       pgTime(snap.CurrentPeriodStart),
			CurrentPeriodEnd:         pgTime(snap.CurrentPeriodEnd),
			CancelAtPeriodEnd:        snap.CancelAtPeriodEnd,
			TrialEnd:                 pgTime(snap.TrialEnd),
			CanceledAt:               pgTime(snap.CanceledAt),
			LastWebhookEventID:       strings.TrimSpace(snap.LastWebhookEventID),
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUserApply(row), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, snap.Principal.Kind)
	}
}

// MarkPastDueForPrincipal flips either org or user state to
// past_due. Mirrors the org-shaped MarkPastDue exactly; the user
// branch hits MarkUserPastDue.
func MarkPastDueForPrincipal(ctx context.Context, deps Deps, p Principal, graceUntil time.Time, eventID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return PrincipalState{}, ErrWebhookEventID
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		state, err := q.MarkPastDue(ctx, deps.Pool, billingdb.MarkPastDueParams{
			OrgID:              p.ID,
			GraceUntil:         pgTime(graceUntil),
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrg(state), nil
	case SubjectKindUser:
		state, err := q.MarkUserPastDue(ctx, deps.Pool, billingdb.MarkUserPastDueParams{
			UserID:             p.ID,
			GraceUntil:         pgTime(graceUntil),
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUser(state), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// MarkCanceledForPrincipal flips either org or user state to
// canceled+free. The user-tier MarkUserCanceled atomically updates
// users.plan='free' via its CTE; the org analog does the same on
// orgs.plan.
func MarkCanceledForPrincipal(ctx context.Context, deps Deps, p Principal, eventID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return PrincipalState{}, ErrWebhookEventID
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		row, err := q.MarkCanceled(ctx, deps.Pool, billingdb.MarkCanceledParams{
			OrgID:              p.ID,
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrgCanceled(row), nil
	case SubjectKindUser:
		row, err := q.MarkUserCanceled(ctx, deps.Pool, billingdb.MarkUserCanceledParams{
			UserID:             p.ID,
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUserCanceled(row), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// MarkPaymentSucceededForPrincipal recovers either org or user from
// past_due/incomplete/unpaid back to active.
func MarkPaymentSucceededForPrincipal(ctx context.Context, deps Deps, p Principal, eventID string) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return PrincipalState{}, ErrWebhookEventID
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		row, err := q.MarkPaymentSucceeded(ctx, deps.Pool, billingdb.MarkPaymentSucceededParams{
			OrgID:              p.ID,
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrgPaymentSucceeded(row), nil
	case SubjectKindUser:
		row, err := q.MarkUserPaymentSucceeded(ctx, deps.Pool, billingdb.MarkUserPaymentSucceededParams{
			UserID:             p.ID,
			LastWebhookEventID: eventID,
		})
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUserPaymentSucceeded(row), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// ClearBillingLockForPrincipal clears the lock columns and returns
// state to none/free as appropriate. Useful for operator-driven
// recovery scenarios.
func ClearBillingLockForPrincipal(ctx context.Context, deps Deps, p Principal) (PrincipalState, error) {
	if err := validateDeps(deps); err != nil {
		return PrincipalState{}, err
	}
	if err := p.Validate(); err != nil {
		return PrincipalState{}, err
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		row, err := q.ClearBillingLock(ctx, deps.Pool, p.ID)
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromOrgClear(row), nil
	case SubjectKindUser:
		row, err := q.ClearUserBillingLock(ctx, deps.Pool, p.ID)
		if err != nil {
			return PrincipalState{}, err
		}
		return principalStateFromUserClear(row), nil
	default:
		return PrincipalState{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// UpsertInvoiceForPrincipal writes an invoice row keyed by the
// resolved subject. The polymorphic billing_invoices schema makes
// the org and user paths identical at the SQL level; this helper
// exists so callers don't reach into the sqlc struct field naming
// drift between OrgID and SubjectID.
func UpsertInvoiceForPrincipal(ctx context.Context, deps Deps, p Principal, snap InvoiceSnapshot) (billingdb.BillingInvoice, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingInvoice{}, err
	}
	if err := p.Validate(); err != nil {
		return billingdb.BillingInvoice{}, err
	}
	snap.StripeInvoiceID = strings.TrimSpace(snap.StripeInvoiceID)
	if snap.StripeInvoiceID == "" {
		return billingdb.BillingInvoice{}, ErrStripeInvoiceID
	}
	snap.StripeCustomerID = strings.TrimSpace(snap.StripeCustomerID)
	if snap.StripeCustomerID == "" {
		return billingdb.BillingInvoice{}, ErrStripeCustomerID
	}
	if !validInvoiceStatus(snap.Status) {
		return billingdb.BillingInvoice{}, fmt.Errorf("%w: %q", ErrInvalidInvoiceStatus, snap.Status)
	}
	// The existing UpsertInvoice sqlc query writes both org_id and
	// (subject_kind, subject_id) from the same `org_id` arg per the
	// 0074 migration's two-step deploy. For user kind we need a
	// polymorphic upsert that DOES NOT write org_id — that surface
	// is added in this sprint as a sibling query when needed. For
	// PRO04 the only user-kind invoice writes come from the webhook
	// handler; org_id stays NULL for those rows per the migration's
	// nullable change.
	switch p.Kind {
	case SubjectKindOrg:
		return billingdb.New().UpsertInvoice(ctx, deps.Pool, billingdb.UpsertInvoiceParams{
			OrgID:                p.ID,
			StripeInvoiceID:      snap.StripeInvoiceID,
			StripeCustomerID:     snap.StripeCustomerID,
			StripeSubscriptionID: pgText(snap.StripeSubscriptionID),
			Status:               snap.Status,
			Number:               strings.TrimSpace(snap.Number),
			Currency:             strings.ToLower(strings.TrimSpace(snap.Currency)),
			AmountDueCents:       snap.AmountDueCents,
			AmountPaidCents:      snap.AmountPaidCents,
			AmountRemainingCents: snap.AmountRemainingCents,
			BillingReason:        strings.TrimSpace(snap.BillingReason),
			HasProration:         snap.HasProration,
			ProrationAmountCents: snap.ProrationAmountCents,
			HostedInvoiceUrl:     strings.TrimSpace(snap.HostedInvoiceURL),
			InvoicePdfUrl:        strings.TrimSpace(snap.InvoicePDFURL),
			PeriodStart:          pgTime(snap.PeriodStart),
			PeriodEnd:            pgTime(snap.PeriodEnd),
			DueAt:                pgTime(snap.DueAt),
			PaidAt:               pgTime(snap.PaidAt),
			VoidedAt:             pgTime(snap.VoidedAt),
		})
	case SubjectKindUser:
		return billingdb.New().UpsertInvoiceForSubject(ctx, deps.Pool, billingdb.UpsertInvoiceForSubjectParams{
			SubjectKind:          billingdb.BillingSubjectKindUser,
			SubjectID:            p.ID,
			StripeInvoiceID:      snap.StripeInvoiceID,
			StripeCustomerID:     snap.StripeCustomerID,
			StripeSubscriptionID: pgText(snap.StripeSubscriptionID),
			Status:               snap.Status,
			Number:               strings.TrimSpace(snap.Number),
			Currency:             strings.ToLower(strings.TrimSpace(snap.Currency)),
			AmountDueCents:       snap.AmountDueCents,
			AmountPaidCents:      snap.AmountPaidCents,
			AmountRemainingCents: snap.AmountRemainingCents,
			BillingReason:        strings.TrimSpace(snap.BillingReason),
			HasProration:         snap.HasProration,
			ProrationAmountCents: snap.ProrationAmountCents,
			HostedInvoiceUrl:     strings.TrimSpace(snap.HostedInvoiceURL),
			InvoicePdfUrl:        strings.TrimSpace(snap.InvoicePDFURL),
			PeriodStart:          pgTime(snap.PeriodStart),
			PeriodEnd:            pgTime(snap.PeriodEnd),
			DueAt:                pgTime(snap.DueAt),
			PaidAt:               pgTime(snap.PaidAt),
			VoidedAt:             pgTime(snap.VoidedAt),
		})
	default:
		return billingdb.BillingInvoice{}, fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
}

// ListInvoicesForPrincipal reads the polymorphic billing_invoices
// table for a given subject. The existing org-shaped
// ListInvoicesForOrg already filters on (subject_kind='org',
// subject_id=$1) under the hood (per the PRO03 query rewrite); the
// kind-agnostic surface here is for PRO04+ callers (the user-side
// settings page in PRO06) that don't want to bind a kind literally.
func ListInvoicesForPrincipal(ctx context.Context, deps Deps, p Principal, limit int32) ([]billingdb.BillingInvoice, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	return billingdb.New().ListInvoicesForSubject(ctx, deps.Pool, billingdb.ListInvoicesForSubjectParams{
		SubjectKind: billingdb.BillingSubjectKind(p.Kind),
		SubjectID:   p.ID,
		Lim:         limit,
	})
}

// ─── internal projections ─────────────────────────────────────────

func principalStateFromOrg(row billingdb.OrgBillingState) PrincipalState {
	out := PrincipalState{
		Principal:          Principal{Kind: SubjectKindOrg, ID: row.OrgID},
		Plan:               string(row.Plan),
		SubscriptionStatus: row.SubscriptionStatus,
		CancelAtPeriodEnd:  row.CancelAtPeriodEnd,
	}
	out.StripeCustomerID = pgTextValue(row.StripeCustomerID)
	out.StripeSubscriptionID = pgTextValue(row.StripeSubscriptionID)
	if row.LockedAt.Valid {
		out.LockedAt = row.LockedAt.Time
	}
	return out
}

func principalStateFromUser(row billingdb.UserBillingState) PrincipalState {
	out := PrincipalState{
		Principal:          Principal{Kind: SubjectKindUser, ID: row.UserID},
		Plan:               string(row.Plan),
		SubscriptionStatus: row.SubscriptionStatus,
		CancelAtPeriodEnd:  row.CancelAtPeriodEnd,
	}
	out.StripeCustomerID = pgTextValue(row.StripeCustomerID)
	out.StripeSubscriptionID = pgTextValue(row.StripeSubscriptionID)
	if row.LockedAt.Valid {
		out.LockedAt = row.LockedAt.Time
	}
	return out
}

func principalStateFromOrgApply(row billingdb.ApplySubscriptionSnapshotRow) PrincipalState {
	return principalStateFromOrg(billingdb.OrgBillingState(row))
}

func principalStateFromUserApply(row billingdb.ApplyUserSubscriptionSnapshotRow) PrincipalState {
	return principalStateFromUser(billingdb.UserBillingState(row))
}

func principalStateFromOrgCanceled(row billingdb.MarkCanceledRow) PrincipalState {
	return principalStateFromOrg(billingdb.OrgBillingState(row))
}

func principalStateFromUserCanceled(row billingdb.MarkUserCanceledRow) PrincipalState {
	return principalStateFromUser(billingdb.UserBillingState(row))
}

func principalStateFromOrgPaymentSucceeded(row billingdb.MarkPaymentSucceededRow) PrincipalState {
	return principalStateFromOrg(billingdb.OrgBillingState(row))
}

func principalStateFromUserPaymentSucceeded(row billingdb.MarkUserPaymentSucceededRow) PrincipalState {
	return principalStateFromUser(billingdb.UserBillingState(row))
}

func principalStateFromOrgClear(row billingdb.ClearBillingLockRow) PrincipalState {
	return principalStateFromOrg(billingdb.OrgBillingState(row))
}

func principalStateFromUserClear(row billingdb.ClearUserBillingLockRow) PrincipalState {
	return principalStateFromUser(billingdb.UserBillingState(row))
}

func pgTextValue(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}
