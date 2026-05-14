// SPDX-License-Identifier: AGPL-3.0-or-later

// Package billing owns local paid-organization state. It stores Stripe
// identifiers and derived subscription state, but it does not call
// Stripe directly; Stripe API details stay in the SP03 adapter layer.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

type Deps struct {
	Pool *pgxpool.Pool
}

type (
	Plan               = billingdb.OrgPlan
	UserPlan           = billingdb.UserPlan
	SubscriptionStatus = billingdb.BillingSubscriptionStatus
	InvoiceStatus      = billingdb.BillingInvoiceStatus
	State              = billingdb.OrgBillingState
	UserState          = billingdb.UserBillingState
)

const (
	PlanFree       = billingdb.OrgPlanFree
	PlanTeam       = billingdb.OrgPlanTeam
	PlanEnterprise = billingdb.OrgPlanEnterprise

	UserPlanFree = billingdb.UserPlanFree
	UserPlanPro  = billingdb.UserPlanPro

	SubscriptionStatusNone       = billingdb.BillingSubscriptionStatusNone
	SubscriptionStatusIncomplete = billingdb.BillingSubscriptionStatusIncomplete
	SubscriptionStatusTrialing   = billingdb.BillingSubscriptionStatusTrialing
	SubscriptionStatusActive     = billingdb.BillingSubscriptionStatusActive
	SubscriptionStatusPastDue    = billingdb.BillingSubscriptionStatusPastDue
	SubscriptionStatusCanceled   = billingdb.BillingSubscriptionStatusCanceled
	SubscriptionStatusUnpaid     = billingdb.BillingSubscriptionStatusUnpaid
	SubscriptionStatusPaused     = billingdb.BillingSubscriptionStatusPaused

	InvoiceStatusDraft         = billingdb.BillingInvoiceStatusDraft
	InvoiceStatusOpen          = billingdb.BillingInvoiceStatusOpen
	InvoiceStatusPaid          = billingdb.BillingInvoiceStatusPaid
	InvoiceStatusVoid          = billingdb.BillingInvoiceStatusVoid
	InvoiceStatusUncollectible = billingdb.BillingInvoiceStatusUncollectible
	InvoiceStatusRefunded      = billingdb.BillingInvoiceStatusRefunded
)

var (
	ErrPoolRequired         = errors.New("billing: pool is required")
	ErrOrgIDRequired        = errors.New("billing: org id is required")
	ErrStripeCustomerID     = errors.New("billing: stripe customer id is required")
	ErrStripeSubscriptionID = errors.New("billing: stripe subscription id is required")
	ErrStripeInvoiceID      = errors.New("billing: stripe invoice id is required")
	ErrInvalidPlan          = errors.New("billing: invalid plan")
	ErrInvalidStatus        = errors.New("billing: invalid subscription status")
	ErrInvalidInvoiceStatus = errors.New("billing: invalid invoice status")
	ErrInvalidSeatCount     = errors.New("billing: seat counts cannot be negative")
	ErrTeamPlanRequired     = errors.New("billing: team plan is required")
	ErrSeatCountBelowUsage  = errors.New("billing: licensed seats cannot be below used seats")
	ErrWebhookEventID       = errors.New("billing: webhook event id is required")
	ErrWebhookEventType     = errors.New("billing: webhook event type is required")
	ErrWebhookPayload       = errors.New("billing: webhook payload must be a JSON object")
)

// SubscriptionSnapshot is the local projection of a provider
// subscription event. Provider-specific conversion belongs in SP03.
type SubscriptionSnapshot struct {
	OrgID                    int64
	Plan                     Plan
	Status                   SubscriptionStatus
	StripeSubscriptionID     string
	StripeSubscriptionItemID string
	CurrentPeriodStart       time.Time
	CurrentPeriodEnd         time.Time
	CancelAtPeriodEnd        bool
	TrialEnd                 time.Time
	CanceledAt               time.Time
	LastWebhookEventID       string
}

type SeatSnapshot struct {
	OrgID                int64
	StripeSubscriptionID string
	LicensedSeats        int
	UsedSeats            int
	Source               string
}

type TeamSeatUsage struct {
	OrgID      int64
	OrgMembers int
	UsedSeats  int
}

type TeamLicenseState struct {
	OrgID                    int64
	Plan                     Plan
	SubscriptionStatus       SubscriptionStatus
	StripeSubscriptionItemID string
	LicensedSeats            int
	UsedSeats                int
	AvailableSeats           int
	SeatSnapshotAt           pgtype.Timestamptz
}

type WebhookEvent struct {
	ProviderEventID string
	EventType       string
	APIVersion      string
	Payload         []byte
}

type InvoiceSnapshot struct {
	OrgID                int64
	StripeInvoiceID      string
	StripeCustomerID     string
	StripeSubscriptionID string
	Status               InvoiceStatus
	Number               string
	Currency             string
	AmountDueCents       int64
	AmountPaidCents      int64
	AmountRemainingCents int64
	HostedInvoiceURL     string
	InvoicePDFURL        string
	PeriodStart          time.Time
	PeriodEnd            time.Time
	DueAt                time.Time
	PaidAt               time.Time
	VoidedAt             time.Time
}

func GetOrgBillingState(ctx context.Context, deps Deps, orgID int64) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	return billingdb.New().GetOrgBillingState(ctx, deps.Pool, orgID)
}

// GetUserBillingState is the user-side counterpart to
// GetOrgBillingState. Returns pgx.ErrNoRows if the user has no
// seeded billing state (shouldn't happen post-PRO03 backfill but
// callers handle defensively).
func GetUserBillingState(ctx context.Context, deps Deps, userID int64) (UserState, error) {
	if err := validateDeps(deps); err != nil {
		return UserState{}, err
	}
	if userID == 0 {
		return UserState{}, ErrOrgIDRequired // reuse: "subject id required"
	}
	return billingdb.New().GetUserBillingState(ctx, deps.Pool, userID)
}

func GetOrgBillingStateByStripeCustomer(ctx context.Context, deps Deps, customerID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return State{}, ErrStripeCustomerID
	}
	return billingdb.New().GetOrgBillingStateByStripeCustomer(ctx, deps.Pool, pgText(customerID))
}

func GetOrgBillingStateByStripeSubscription(ctx context.Context, deps Deps, subscriptionID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return State{}, ErrStripeSubscriptionID
	}
	return billingdb.New().GetOrgBillingStateByStripeSubscription(ctx, deps.Pool, pgText(subscriptionID))
}

func SetStripeCustomer(ctx context.Context, deps Deps, orgID int64, customerID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return State{}, ErrStripeCustomerID
	}
	return billingdb.New().SetStripeCustomer(ctx, deps.Pool, billingdb.SetStripeCustomerParams{
		OrgID:            orgID,
		StripeCustomerID: pgText(customerID),
	})
}

func ApplySubscriptionSnapshot(ctx context.Context, deps Deps, snap SubscriptionSnapshot) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if snap.OrgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	if !validPlan(snap.Plan) {
		return State{}, fmt.Errorf("%w: %q", ErrInvalidPlan, snap.Plan)
	}
	if !validStatus(snap.Status) {
		return State{}, fmt.Errorf("%w: %q", ErrInvalidStatus, snap.Status)
	}
	row, err := billingdb.New().ApplySubscriptionSnapshot(ctx, deps.Pool, billingdb.ApplySubscriptionSnapshotParams{
		OrgID:                    snap.OrgID,
		Plan:                     snap.Plan,
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
		return State{}, err
	}
	return stateFromApply(row), nil
}

func RecordWebhookEvent(ctx context.Context, deps Deps, event WebhookEvent) (billingdb.BillingWebhookEvent, bool, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingWebhookEvent{}, false, err
	}
	event.ProviderEventID = strings.TrimSpace(event.ProviderEventID)
	event.EventType = strings.TrimSpace(event.EventType)
	event.APIVersion = strings.TrimSpace(event.APIVersion)
	if event.ProviderEventID == "" {
		return billingdb.BillingWebhookEvent{}, false, ErrWebhookEventID
	}
	if event.EventType == "" {
		return billingdb.BillingWebhookEvent{}, false, ErrWebhookEventType
	}
	if !jsonObject(event.Payload) {
		return billingdb.BillingWebhookEvent{}, false, ErrWebhookPayload
	}
	row, err := billingdb.New().CreateWebhookEventReceipt(ctx, deps.Pool, billingdb.CreateWebhookEventReceiptParams{
		ProviderEventID: event.ProviderEventID,
		EventType:       event.EventType,
		ApiVersion:      event.APIVersion,
		Payload:         event.Payload,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = billingdb.New().GetWebhookEventReceipt(ctx, deps.Pool, event.ProviderEventID)
			if err != nil {
				return billingdb.BillingWebhookEvent{}, false, err
			}
			return row, false, nil
		}
		return billingdb.BillingWebhookEvent{}, false, err
	}
	return row, true, nil
}

func MarkWebhookEventProcessed(ctx context.Context, deps Deps, providerEventID string) (billingdb.BillingWebhookEvent, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingWebhookEvent{}, err
	}
	providerEventID = strings.TrimSpace(providerEventID)
	if providerEventID == "" {
		return billingdb.BillingWebhookEvent{}, ErrWebhookEventID
	}
	return billingdb.New().MarkWebhookEventProcessed(ctx, deps.Pool, providerEventID)
}

func MarkWebhookEventFailed(ctx context.Context, deps Deps, providerEventID, processError string) (billingdb.BillingWebhookEvent, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingWebhookEvent{}, err
	}
	providerEventID = strings.TrimSpace(providerEventID)
	if providerEventID == "" {
		return billingdb.BillingWebhookEvent{}, ErrWebhookEventID
	}
	processError = strings.TrimSpace(processError)
	if len(processError) > 2000 {
		processError = processError[:2000]
	}
	return billingdb.New().MarkWebhookEventFailed(ctx, deps.Pool, billingdb.MarkWebhookEventFailedParams{
		ProviderEventID: providerEventID,
		ProcessError:    processError,
	})
}

func GetWebhookEventReceipt(ctx context.Context, deps Deps, providerEventID string) (billingdb.BillingWebhookEvent, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingWebhookEvent{}, err
	}
	providerEventID = strings.TrimSpace(providerEventID)
	if providerEventID == "" {
		return billingdb.BillingWebhookEvent{}, ErrWebhookEventID
	}
	return billingdb.New().GetWebhookEventReceipt(ctx, deps.Pool, providerEventID)
}

// SetWebhookEventSubjectForPrincipal records the resolved subject on
// the receipt row. Called after a successful subject-resolution step
// in the webhook apply path (before guard + state mutation) so the
// audit trail survives even if the apply later fails. Migration 0075's
// CHECK constraint enforces both-or-neither — the helper rejects a
// zero principal.
func SetWebhookEventSubjectForPrincipal(ctx context.Context, deps Deps, providerEventID string, p Principal) error {
	if err := validateDeps(deps); err != nil {
		return err
	}
	providerEventID = strings.TrimSpace(providerEventID)
	if providerEventID == "" {
		return ErrWebhookEventID
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return billingdb.New().SetWebhookEventSubject(ctx, deps.Pool, billingdb.SetWebhookEventSubjectParams{
		SubjectKind:     billingdb.BillingSubjectKind(p.Kind),
		SubjectID:       p.ID,
		ProviderEventID: providerEventID,
	})
}

// MarkInvoiceRefunded flips a billing_invoices row to status='refunded'
// and stamps refunded_at. PRO08 D2: surface a Stripe-side refund in
// shithub's billing settings UI. The Stripe invoice itself stays
// status='paid' after a refund; shithub maintains its own UI surface.
// Returns pgx.ErrNoRows when the invoice id isn't on file (Stripe
// refunded an invoice we never recorded — operator should reconcile).
func MarkInvoiceRefunded(ctx context.Context, deps Deps, stripeInvoiceID string) (billingdb.BillingInvoice, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingInvoice{}, err
	}
	stripeInvoiceID = strings.TrimSpace(stripeInvoiceID)
	if stripeInvoiceID == "" {
		return billingdb.BillingInvoice{}, ErrStripeInvoiceID
	}
	return billingdb.New().MarkInvoiceRefunded(ctx, deps.Pool, stripeInvoiceID)
}

// IsBillingEventStaleForPrincipal reports whether an incoming Stripe
// event's timestamp is older than the last event we've already applied
// for this principal. PRO08 D4: the handler refuses stale events so
// reverse-ordered retries can't regress state (e.g., a stale
// subscription.updated[active] arriving after a fresh
// subscription.updated[canceled] re-activating the principal).
//
// Returns false when there's no prior event on file (the first event
// is never stale) or when the row simply doesn't exist (defaults to
// allow; the caller's own ErrNoRows path handles missing-state).
func IsBillingEventStaleForPrincipal(ctx context.Context, deps Deps, p Principal, eventAt time.Time) (bool, error) {
	if err := validateDeps(deps); err != nil {
		return false, err
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	if eventAt.IsZero() {
		return false, nil
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		stale, err := q.IsOrgBillingEventStale(ctx, deps.Pool, billingdb.IsOrgBillingEventStaleParams{
			OrgID:   p.ID,
			EventAt: pgTime(eventAt),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return stale, nil
	case SubjectKindUser:
		stale, err := q.IsUserBillingEventStale(ctx, deps.Pool, billingdb.IsUserBillingEventStaleParams{
			UserID:  p.ID,
			EventAt: pgTime(eventAt),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return stale, nil
	}
	return false, nil
}

// TouchBillingLastEventAtForPrincipal bumps last_event_at on the
// principal's billing-state row. PRO08 D4: called after a successful
// apply so subsequent staleness checks have a baseline. The query
// uses GREATEST(prev, incoming) so an out-of-order-but-recent retry
// doesn't regress the timestamp.
func TouchBillingLastEventAtForPrincipal(ctx context.Context, deps Deps, p Principal, eventAt time.Time) error {
	if err := validateDeps(deps); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if eventAt.IsZero() {
		return nil
	}
	q := billingdb.New()
	switch p.Kind {
	case SubjectKindOrg:
		return q.TouchOrgBillingLastEventAt(ctx, deps.Pool, billingdb.TouchOrgBillingLastEventAtParams{
			OrgID:   p.ID,
			EventAt: pgTime(eventAt),
		})
	case SubjectKindUser:
		return q.TouchUserBillingLastEventAt(ctx, deps.Pool, billingdb.TouchUserBillingLastEventAtParams{
			UserID:  p.ID,
			EventAt: pgTime(eventAt),
		})
	}
	return nil
}

// ListFailedWebhookEvents is the operator query for "events we
// received but failed to process." Returns rows whose process_error
// is non-empty OR that have any processing_attempts but no
// processed_at (in-flight failures). Returned in descending received_at
// order; limit caps the result set.
func ListFailedWebhookEvents(ctx context.Context, deps Deps, limit int32) ([]billingdb.ListFailedWebhookEventsRow, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	return billingdb.New().ListFailedWebhookEvents(ctx, deps.Pool, limit)
}

func UpsertInvoice(ctx context.Context, deps Deps, snap InvoiceSnapshot) (billingdb.BillingInvoice, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingInvoice{}, err
	}
	if snap.OrgID == 0 {
		return billingdb.BillingInvoice{}, ErrOrgIDRequired
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
	row, err := billingdb.New().UpsertInvoice(ctx, deps.Pool, billingdb.UpsertInvoiceParams{
		OrgID:                snap.OrgID,
		StripeInvoiceID:      snap.StripeInvoiceID,
		StripeCustomerID:     snap.StripeCustomerID,
		StripeSubscriptionID: pgText(snap.StripeSubscriptionID),
		Status:               snap.Status,
		Number:               strings.TrimSpace(snap.Number),
		Currency:             strings.ToLower(strings.TrimSpace(snap.Currency)),
		AmountDueCents:       snap.AmountDueCents,
		AmountPaidCents:      snap.AmountPaidCents,
		AmountRemainingCents: snap.AmountRemainingCents,
		HostedInvoiceUrl:     strings.TrimSpace(snap.HostedInvoiceURL),
		InvoicePdfUrl:        strings.TrimSpace(snap.InvoicePDFURL),
		PeriodStart:          pgTime(snap.PeriodStart),
		PeriodEnd:            pgTime(snap.PeriodEnd),
		DueAt:                pgTime(snap.DueAt),
		PaidAt:               pgTime(snap.PaidAt),
		VoidedAt:             pgTime(snap.VoidedAt),
	})
	if err != nil {
		return billingdb.BillingInvoice{}, err
	}
	return row, nil
}

func ListInvoicesForOrg(ctx context.Context, deps Deps, orgID int64, limit int32) ([]billingdb.BillingInvoice, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if orgID == 0 {
		return nil, ErrOrgIDRequired
	}
	if limit <= 0 {
		limit = 10
	}
	return billingdb.New().ListInvoicesForOrg(ctx, deps.Pool, billingdb.ListInvoicesForOrgParams{
		// SubjectID equals OrgID by the billing_invoices_org_id_matches_subject
		// CHECK constraint added in migration 0074. The polymorphic shape lets
		// PRO04+ callers reuse this query without a fork.
		SubjectID: orgID,
		Limit:     limit,
	})
}

func SyncSeatSnapshot(ctx context.Context, deps Deps, snap SeatSnapshot) (billingdb.BillingSeatSnapshot, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingSeatSnapshot{}, err
	}
	if snap.OrgID == 0 {
		return billingdb.BillingSeatSnapshot{}, ErrOrgIDRequired
	}
	if snap.LicensedSeats < 0 || snap.UsedSeats < 0 {
		return billingdb.BillingSeatSnapshot{}, ErrInvalidSeatCount
	}
	source := strings.TrimSpace(snap.Source)
	if source == "" {
		source = "local"
	}
	row, err := billingdb.New().CreateSeatSnapshot(ctx, deps.Pool, billingdb.CreateSeatSnapshotParams{
		OrgID:                snap.OrgID,
		StripeSubscriptionID: pgText(snap.StripeSubscriptionID),
		LicensedSeats:        int32(snap.LicensedSeats),
		UsedSeats:            int32(snap.UsedSeats),
		Source:               source,
	})
	if err != nil {
		return billingdb.BillingSeatSnapshot{}, err
	}
	return billingdb.BillingSeatSnapshot(row), nil
}

func ComputeTeamSeatUsage(ctx context.Context, deps Deps, orgID int64) (TeamSeatUsage, error) {
	if err := validateDeps(deps); err != nil {
		return TeamSeatUsage{}, err
	}
	if orgID == 0 {
		return TeamSeatUsage{}, ErrOrgIDRequired
	}
	members, err := CountBillableOrgMembers(ctx, deps, orgID)
	if err != nil {
		return TeamSeatUsage{}, err
	}
	return TeamSeatUsage{
		OrgID:      orgID,
		OrgMembers: members,
		UsedSeats:  members,
	}, nil
}

func GetTeamLicenseState(ctx context.Context, deps Deps, orgID int64) (TeamLicenseState, error) {
	if err := validateDeps(deps); err != nil {
		return TeamLicenseState{}, err
	}
	if orgID == 0 {
		return TeamLicenseState{}, ErrOrgIDRequired
	}
	state, err := GetOrgBillingState(ctx, deps, orgID)
	if err != nil {
		return TeamLicenseState{}, err
	}
	usage, err := ComputeTeamSeatUsage(ctx, deps, orgID)
	if err != nil {
		return TeamLicenseState{}, err
	}
	licensed := int(state.LicensedSeats)
	if state.Plan != PlanTeam {
		licensed = 0
	}
	available := licensed - usage.UsedSeats
	if available < 0 {
		available = 0
	}
	subscriptionItemID := ""
	if state.StripeSubscriptionItemID.Valid {
		subscriptionItemID = strings.TrimSpace(state.StripeSubscriptionItemID.String)
	}
	return TeamLicenseState{
		OrgID:                    orgID,
		Plan:                     state.Plan,
		SubscriptionStatus:       state.SubscriptionStatus,
		StripeSubscriptionItemID: subscriptionItemID,
		LicensedSeats:            licensed,
		UsedSeats:                usage.UsedSeats,
		AvailableSeats:           available,
		SeatSnapshotAt:           state.SeatSnapshotAt,
	}, nil
}

func SetTeamLicensedSeats(ctx context.Context, deps Deps, orgID int64, licensedSeats int, source string) (TeamLicenseState, error) {
	if err := validateDeps(deps); err != nil {
		return TeamLicenseState{}, err
	}
	if orgID == 0 {
		return TeamLicenseState{}, ErrOrgIDRequired
	}
	if licensedSeats < 1 {
		return TeamLicenseState{}, ErrInvalidSeatCount
	}
	state, err := GetOrgBillingState(ctx, deps, orgID)
	if err != nil {
		return TeamLicenseState{}, err
	}
	if state.Plan != PlanTeam {
		return TeamLicenseState{}, ErrTeamPlanRequired
	}
	usage, err := ComputeTeamSeatUsage(ctx, deps, orgID)
	if err != nil {
		return TeamLicenseState{}, err
	}
	if licensedSeats < usage.UsedSeats {
		return TeamLicenseState{}, ErrSeatCountBelowUsage
	}
	if _, err := SyncSeatSnapshot(ctx, deps, SeatSnapshot{
		OrgID:                orgID,
		StripeSubscriptionID: state.StripeSubscriptionID.String,
		LicensedSeats:        licensedSeats,
		UsedSeats:            usage.UsedSeats,
		Source:               source,
	}); err != nil {
		return TeamLicenseState{}, err
	}
	return GetTeamLicenseState(ctx, deps, orgID)
}

func CountBillableOrgMembers(ctx context.Context, deps Deps, orgID int64) (int, error) {
	if err := validateDeps(deps); err != nil {
		return 0, err
	}
	if orgID == 0 {
		return 0, ErrOrgIDRequired
	}
	n, err := billingdb.New().CountBillableOrgMembers(ctx, deps.Pool, orgID)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func CountPendingOrgInvitations(ctx context.Context, deps Deps, orgID int64) (int, error) {
	if err := validateDeps(deps); err != nil {
		return 0, err
	}
	if orgID == 0 {
		return 0, ErrOrgIDRequired
	}
	n, err := billingdb.New().CountPendingOrgInvitations(ctx, deps.Pool, orgID)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func MarkPastDue(ctx context.Context, deps Deps, orgID int64, graceUntil time.Time, lastWebhookEventID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	return billingdb.New().MarkPastDue(ctx, deps.Pool, billingdb.MarkPastDueParams{
		OrgID:              orgID,
		GraceUntil:         pgTime(graceUntil),
		LastWebhookEventID: strings.TrimSpace(lastWebhookEventID),
	})
}

func MarkPaymentSucceeded(ctx context.Context, deps Deps, orgID int64, lastWebhookEventID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	row, err := billingdb.New().MarkPaymentSucceeded(ctx, deps.Pool, billingdb.MarkPaymentSucceededParams{
		OrgID:              orgID,
		LastWebhookEventID: strings.TrimSpace(lastWebhookEventID),
	})
	if err != nil {
		return State{}, err
	}
	return stateFromPaymentSucceeded(row), nil
}

func MarkCanceled(ctx context.Context, deps Deps, orgID int64, lastWebhookEventID string) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	row, err := billingdb.New().MarkCanceled(ctx, deps.Pool, billingdb.MarkCanceledParams{
		OrgID:              orgID,
		LastWebhookEventID: strings.TrimSpace(lastWebhookEventID),
	})
	if err != nil {
		return State{}, err
	}
	return stateFromCanceled(row), nil
}

func ClearBillingLock(ctx context.Context, deps Deps, orgID int64) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	row, err := billingdb.New().ClearBillingLock(ctx, deps.Pool, orgID)
	if err != nil {
		return State{}, err
	}
	return stateFromClear(row), nil
}

func validateDeps(deps Deps) error {
	if deps.Pool == nil {
		return ErrPoolRequired
	}
	return nil
}

func validPlan(plan Plan) bool {
	switch plan {
	case PlanFree, PlanTeam, PlanEnterprise:
		return true
	default:
		return false
	}
}

func validStatus(status SubscriptionStatus) bool {
	switch status {
	case SubscriptionStatusNone,
		SubscriptionStatusIncomplete,
		SubscriptionStatusTrialing,
		SubscriptionStatusActive,
		SubscriptionStatusPastDue,
		SubscriptionStatusCanceled,
		SubscriptionStatusUnpaid,
		SubscriptionStatusPaused:
		return true
	default:
		return false
	}
}

func validInvoiceStatus(status InvoiceStatus) bool {
	switch status {
	case InvoiceStatusDraft,
		InvoiceStatusOpen,
		InvoiceStatusPaid,
		InvoiceStatusVoid,
		InvoiceStatusUncollectible,
		InvoiceStatusRefunded:
		return true
	default:
		return false
	}
}

func pgText(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	return pgtype.Text{String: s, Valid: s != ""}
}

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func jsonObject(payload []byte) bool {
	var v map[string]any
	return json.Unmarshal(payload, &v) == nil && v != nil
}

func stateFromApply(row billingdb.ApplySubscriptionSnapshotRow) State {
	return State(row)
}

func stateFromCanceled(row billingdb.MarkCanceledRow) State {
	return State(row)
}

func stateFromPaymentSucceeded(row billingdb.MarkPaymentSucceededRow) State {
	return State(row)
}

func stateFromClear(row billingdb.ClearBillingLockRow) State {
	return State(row)
}
