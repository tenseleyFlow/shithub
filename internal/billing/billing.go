// SPDX-License-Identifier: AGPL-3.0-or-later

// Package billing owns local paid-organization state. It stores Stripe
// identifiers and derived subscription state, but it does not call
// Stripe directly; webhook/API integration lands in SP03.
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
)

var (
	ErrPoolRequired     = errors.New("billing: pool is required")
	ErrOrgIDRequired    = errors.New("billing: org id is required")
	ErrStripeCustomerID = errors.New("billing: stripe customer id is required")
	ErrInvalidPlan      = errors.New("billing: invalid plan")
	ErrInvalidStatus    = errors.New("billing: invalid subscription status")
	ErrInvalidSeatCount = errors.New("billing: seat counts cannot be negative")
	ErrWebhookEventID   = errors.New("billing: webhook event id is required")
	ErrWebhookEventType = errors.New("billing: webhook event type is required")
	ErrWebhookPayload   = errors.New("billing: webhook payload must be a JSON object")
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

func GetOrgBillingState(ctx context.Context, deps Deps, orgID int64) (State, error) {
	if err := validateDeps(deps); err != nil {
		return State{}, err
	}
	if orgID == 0 {
		return State{}, ErrOrgIDRequired
	}
	return billingdb.New().GetOrgBillingState(ctx, deps.Pool, orgID)
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
			return billingdb.BillingWebhookEvent{}, false, nil
		}
		return billingdb.BillingWebhookEvent{}, false, err
	}
	return row, true, nil
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
