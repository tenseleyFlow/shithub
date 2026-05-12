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
	SubscriptionStatus = billingdb.BillingSubscriptionStatus
	InvoiceStatus      = billingdb.BillingInvoiceStatus
	State              = billingdb.OrgBillingState
)

const (
	PlanFree       = billingdb.OrgPlanFree
	PlanTeam       = billingdb.OrgPlanTeam
	PlanEnterprise = billingdb.OrgPlanEnterprise

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
	ActiveMembers        int
	BillableSeats        int
	Source               string
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
		OrgID: orgID,
		Limit: limit,
	})
}

func SyncSeatSnapshot(ctx context.Context, deps Deps, snap SeatSnapshot) (billingdb.BillingSeatSnapshot, error) {
	if err := validateDeps(deps); err != nil {
		return billingdb.BillingSeatSnapshot{}, err
	}
	if snap.OrgID == 0 {
		return billingdb.BillingSeatSnapshot{}, ErrOrgIDRequired
	}
	if snap.ActiveMembers < 0 || snap.BillableSeats < 0 {
		return billingdb.BillingSeatSnapshot{}, ErrInvalidSeatCount
	}
	source := strings.TrimSpace(snap.Source)
	if source == "" {
		source = "local"
	}
	row, err := billingdb.New().CreateSeatSnapshot(ctx, deps.Pool, billingdb.CreateSeatSnapshotParams{
		OrgID:                snap.OrgID,
		StripeSubscriptionID: pgText(snap.StripeSubscriptionID),
		ActiveMembers:        int32(snap.ActiveMembers),
		BillableSeats:        int32(snap.BillableSeats),
		Source:               source,
	})
	if err != nil {
		return billingdb.BillingSeatSnapshot{}, err
	}
	return billingdb.BillingSeatSnapshot(row), nil
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
		InvoiceStatusUncollectible:
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

func stateFromClear(row billingdb.ClearBillingLockRow) State {
	return State(row)
}
