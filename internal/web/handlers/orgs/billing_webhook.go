// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
)

const stripeWebhookBodyLimit = 1 << 20

func (h *Handlers) billingWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.billingConfigured() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, stripeWebhookBodyLimit)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	event, err := h.d.Stripe.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"))
	if err != nil {
		http.Error(w, "invalid stripe signature", http.StatusBadRequest)
		return
	}
	receipt, created, err := orgbilling.RecordWebhookEvent(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgbilling.WebhookEvent{
		ProviderEventID: event.ID,
		EventType:       string(event.Type),
		APIVersion:      event.APIVersion,
		Payload:         payload,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: record webhook receipt", "event_id", event.ID, "event_type", event.Type, "error", err)
		http.Error(w, "could not record webhook receipt", http.StatusInternalServerError)
		return
	}
	if !created && receipt.ProcessedAt.Valid {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if err := h.processStripeWebhook(r.Context(), event); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: process webhook", "event_id", event.ID, "event_type", event.Type, "error", err)
		if _, markErr := orgbilling.MarkWebhookEventFailed(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, event.ID, err.Error()); markErr != nil {
			h.d.Logger.ErrorContext(r.Context(), "org billing: mark webhook failed", "event_id", event.ID, "error", markErr)
		}
		http.Error(w, "webhook processing failed", http.StatusInternalServerError)
		return
	}
	if _, err := orgbilling.MarkWebhookEventProcessed(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, event.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: mark webhook processed", "event_id", event.ID, "error", err)
		http.Error(w, "could not finalize webhook receipt", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handlers) processStripeWebhook(ctx context.Context, event stripeapi.Event) error {
	switch string(event.Type) {
	case "checkout.session.completed":
		return h.applyStripeCheckoutCompleted(ctx, event)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		return h.applyStripeSubscriptionEvent(ctx, event)
	case "invoice.payment_succeeded", "invoice.payment_failed", "invoice.voided", "invoice.marked_uncollectible":
		return h.applyStripeInvoiceEvent(ctx, event)
	default:
		return nil
	}
}

func (h *Handlers) applyStripeCheckoutCompleted(ctx context.Context, event stripeapi.Event) error {
	var session stripeapi.CheckoutSession
	if err := unmarshalStripeEventObject(event, &session); err != nil {
		return err
	}
	orgID := stripeOrgIDFromMetadata(session.Metadata)
	if orgID == 0 {
		if id, err := strconv.ParseInt(strings.TrimSpace(session.ClientReferenceID), 10, 64); err == nil && id > 0 {
			orgID = id
		}
	}
	if orgID == 0 {
		return errors.New("stripe checkout.session.completed missing shithub org metadata")
	}
	customerID := stripeCustomerID(session.Customer)
	if customerID == "" {
		return errors.New("stripe checkout.session.completed missing customer")
	}
	_, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgID, customerID)
	return err
}

func (h *Handlers) applyStripeSubscriptionEvent(ctx context.Context, event stripeapi.Event) error {
	var sub stripeapi.Subscription
	if err := unmarshalStripeEventObject(event, &sub); err != nil {
		return err
	}
	orgID, err := h.resolveOrgIDFromSubscription(ctx, &sub)
	if err != nil {
		return err
	}
	customerID := stripeCustomerID(sub.Customer)
	if customerID != "" {
		if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgID, customerID); err != nil {
			return err
		}
	}
	status, err := stripeSubscriptionStatus(sub.Status)
	if err != nil {
		return err
	}
	if status == orgbilling.SubscriptionStatusCanceled || string(event.Type) == "customer.subscription.deleted" {
		_, err := orgbilling.MarkCanceled(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgID, event.ID)
		return err
	}
	itemID := stripeSubscriptionItemID(sub.Items)
	periodStart, periodEnd := stripeSubscriptionPeriod(sub.Items)
	_, err = orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   status,
		StripeSubscriptionID:     strings.TrimSpace(sub.ID),
		StripeSubscriptionItemID: itemID,
		CurrentPeriodStart:       periodStart,
		CurrentPeriodEnd:         periodEnd,
		CancelAtPeriodEnd:        sub.CancelAtPeriodEnd,
		TrialEnd:                 unixTime(sub.TrialEnd),
		CanceledAt:               unixTime(sub.CanceledAt),
		LastWebhookEventID:       event.ID,
	})
	return err
}

func (h *Handlers) applyStripeInvoiceEvent(ctx context.Context, event stripeapi.Event) error {
	var inv stripeapi.Invoice
	if err := unmarshalStripeEventObject(event, &inv); err != nil {
		return err
	}
	orgID, state, err := h.resolveOrgStateFromInvoice(ctx, &inv)
	if err != nil {
		return err
	}
	status, err := stripeInvoiceStatus(inv.Status)
	if err != nil {
		return err
	}
	if _, err := orgbilling.UpsertInvoice(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgbilling.InvoiceSnapshot{
		OrgID:                orgID,
		StripeInvoiceID:      strings.TrimSpace(inv.ID),
		StripeCustomerID:     stripeCustomerID(inv.Customer),
		StripeSubscriptionID: stripeInvoiceSubscriptionID(&inv),
		Status:               status,
		Number:               strings.TrimSpace(inv.Number),
		Currency:             strings.ToLower(string(inv.Currency)),
		AmountDueCents:       inv.AmountDue,
		AmountPaidCents:      inv.AmountPaid,
		AmountRemainingCents: inv.AmountRemaining,
		HostedInvoiceURL:     strings.TrimSpace(inv.HostedInvoiceURL),
		InvoicePDFURL:        strings.TrimSpace(inv.InvoicePDF),
		PeriodStart:          unixTime(inv.PeriodStart),
		PeriodEnd:            unixTime(inv.PeriodEnd),
		DueAt:                unixTime(inv.DueDate),
		PaidAt:               unixTime(stripeInvoicePaidAt(inv.StatusTransitions)),
		VoidedAt:             unixTime(stripeInvoiceVoidedAt(inv.StatusTransitions)),
	}); err != nil {
		return err
	}
	switch string(event.Type) {
	case "invoice.payment_failed":
		graceUntil := time.Now().UTC().Add(h.d.BillingGracePeriod)
		_, err := orgbilling.MarkPastDue(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgID, graceUntil, event.ID)
		return err
	case "invoice.payment_succeeded":
		if state.SubscriptionStatus != orgbilling.SubscriptionStatusCanceled {
			_, err := orgbilling.ClearBillingLock(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgID)
			return err
		}
	}
	return nil
}

func (h *Handlers) resolveOrgIDFromSubscription(ctx context.Context, sub *stripeapi.Subscription) (int64, error) {
	if orgID := stripeOrgIDFromMetadata(sub.Metadata); orgID != 0 {
		return orgID, nil
	}
	if customerID := stripeCustomerID(sub.Customer); customerID != "" {
		state, err := orgbilling.GetOrgBillingStateByStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, customerID)
		if err == nil {
			return state.OrgID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	if subID := strings.TrimSpace(sub.ID); subID != "" {
		state, err := orgbilling.GetOrgBillingStateByStripeSubscription(ctx, orgbilling.Deps{Pool: h.d.Pool}, subID)
		if err == nil {
			return state.OrgID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	return 0, errors.New("stripe subscription does not map to a shithub organization")
}

func (h *Handlers) resolveOrgStateFromInvoice(ctx context.Context, inv *stripeapi.Invoice) (int64, orgbilling.State, error) {
	if customerID := stripeCustomerID(inv.Customer); customerID != "" {
		state, err := orgbilling.GetOrgBillingStateByStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, customerID)
		if err == nil {
			return state.OrgID, state, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, orgbilling.State{}, err
		}
	}
	if subID := stripeInvoiceSubscriptionID(inv); subID != "" {
		state, err := orgbilling.GetOrgBillingStateByStripeSubscription(ctx, orgbilling.Deps{Pool: h.d.Pool}, subID)
		if err == nil {
			return state.OrgID, state, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, orgbilling.State{}, err
		}
	}
	return 0, orgbilling.State{}, errors.New("stripe invoice does not map to a shithub organization")
}

func stripeOrgIDFromMetadata(metadata map[string]string) int64 {
	raw := strings.TrimSpace(metadata[stripebilling.MetadataOrgID])
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func stripeCustomerID(customer *stripeapi.Customer) string {
	if customer == nil {
		return ""
	}
	return strings.TrimSpace(customer.ID)
}

func stripeSubscriptionID(sub *stripeapi.Subscription) string {
	if sub == nil {
		return ""
	}
	return strings.TrimSpace(sub.ID)
}

func stripeSubscriptionItemID(items *stripeapi.SubscriptionItemList) string {
	if items == nil || len(items.Data) == 0 || items.Data[0] == nil {
		return ""
	}
	return strings.TrimSpace(items.Data[0].ID)
}

func stripeSubscriptionPeriod(items *stripeapi.SubscriptionItemList) (time.Time, time.Time) {
	if items == nil || len(items.Data) == 0 || items.Data[0] == nil {
		return time.Time{}, time.Time{}
	}
	return unixTime(items.Data[0].CurrentPeriodStart), unixTime(items.Data[0].CurrentPeriodEnd)
}

func stripeInvoiceSubscriptionID(inv *stripeapi.Invoice) string {
	if inv == nil || inv.Parent == nil || inv.Parent.SubscriptionDetails == nil || inv.Parent.SubscriptionDetails.Subscription == nil {
		return ""
	}
	return strings.TrimSpace(inv.Parent.SubscriptionDetails.Subscription.ID)
}

func stripeSubscriptionStatus(status stripeapi.SubscriptionStatus) (orgbilling.SubscriptionStatus, error) {
	switch string(status) {
	case "incomplete":
		return orgbilling.SubscriptionStatusIncomplete, nil
	case "trialing":
		return orgbilling.SubscriptionStatusTrialing, nil
	case "active":
		return orgbilling.SubscriptionStatusActive, nil
	case "past_due":
		return orgbilling.SubscriptionStatusPastDue, nil
	case "canceled":
		return orgbilling.SubscriptionStatusCanceled, nil
	case "unpaid":
		return orgbilling.SubscriptionStatusUnpaid, nil
	case "paused":
		return orgbilling.SubscriptionStatusPaused, nil
	case "incomplete_expired":
		return orgbilling.SubscriptionStatusCanceled, nil
	default:
		return "", fmt.Errorf("unsupported stripe subscription status %q", status)
	}
}

func stripeInvoiceStatus(status stripeapi.InvoiceStatus) (orgbilling.InvoiceStatus, error) {
	switch string(status) {
	case "draft":
		return orgbilling.InvoiceStatusDraft, nil
	case "open":
		return orgbilling.InvoiceStatusOpen, nil
	case "paid":
		return orgbilling.InvoiceStatusPaid, nil
	case "void":
		return orgbilling.InvoiceStatusVoid, nil
	case "uncollectible":
		return orgbilling.InvoiceStatusUncollectible, nil
	default:
		return "", fmt.Errorf("unsupported stripe invoice status %q", status)
	}
}

func stripeInvoicePaidAt(transitions *stripeapi.InvoiceStatusTransitions) int64 {
	if transitions == nil {
		return 0
	}
	return transitions.PaidAt
}

func stripeInvoiceVoidedAt(transitions *stripeapi.InvoiceStatusTransitions) int64 {
	if transitions == nil {
		return 0
	}
	return transitions.VoidedAt
}

func unixTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

func unmarshalStripeEventObject[T any](event stripeapi.Event, out *T) error {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return errors.New("stripe webhook missing event data")
	}
	if err := json.Unmarshal(event.Data.Raw, out); err != nil {
		return fmt.Errorf("decode stripe %s event: %w", event.Type, err)
	}
	return nil
}
