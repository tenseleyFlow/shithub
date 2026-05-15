// SPDX-License-Identifier: AGPL-3.0-or-later

// PRO04 note: this file is now subject-agnostic — it routes Stripe
// webhook events to either org or user billing state based on the
// resolved Principal. The file still lives under `handlers/orgs/`
// for wiring continuity; a follow-up sprint moves it to
// `handlers/billing/` once the SP-only callers are gone.

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
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
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
		observeBillingWebhook("unknown", "invalid_body")
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	event, err := h.d.Stripe.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"))
	if err != nil {
		observeBillingWebhook("unknown", "verify_failed")
		http.Error(w, "invalid stripe signature", http.StatusBadRequest)
		return
	}
	// PRO08 A3: serialize concurrent deliveries of the same event_id
	// with a session-scoped advisory lock on a dedicated pool conn.
	// Without this, the dedup short-circuit at "!created && processed_at"
	// races: two replays both observe processed_at=NULL, both apply,
	// double-mutating state. The lock makes the apply path mutually
	// exclusive per event_id. A racing replay returns 200 immediately —
	// Stripe stops retrying THIS delivery; if this worker fails the
	// apply, Stripe will resend later (different delivery, fresh race).
	conn, err := h.d.Pool.Acquire(r.Context())
	if err != nil {
		observeBillingWebhook(string(event.Type), "lock_failed")
		h.d.Logger.ErrorContext(r.Context(), "org billing: acquire conn for webhook lock", "event_id", event.ID, "error", err)
		http.Error(w, "could not acquire webhook lock", http.StatusInternalServerError)
		return
	}
	defer conn.Release()
	var acquired bool
	if err := conn.QueryRow(r.Context(), "SELECT pg_try_advisory_lock(hashtext($1)::bigint)", event.ID).Scan(&acquired); err != nil {
		observeBillingWebhook(string(event.Type), "lock_failed")
		h.d.Logger.ErrorContext(r.Context(), "org billing: try advisory lock", "event_id", event.ID, "error", err)
		http.Error(w, "could not acquire webhook lock", http.StatusInternalServerError)
		return
	}
	if !acquired {
		// Another worker holds the lock — return 200 so Stripe stops
		// retrying THIS delivery; the in-flight worker finishes the apply.
		h.d.Logger.InfoContext(r.Context(), "org billing: webhook in flight elsewhere",
			"event_id", event.ID, "event_type", event.Type)
		observeBillingWebhook(string(event.Type), "in_flight")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok (in flight)"))
		return
	}
	defer func() {
		// Use Background so unlock runs even if request ctx cancelled.
		// Best-effort — txn cleanup eventually releases either way.
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1)::bigint)", event.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "org billing: advisory unlock", "event_id", event.ID, "error", err)
		}
	}()
	receipt, created, err := orgbilling.RecordWebhookEvent(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgbilling.WebhookEvent{
		ProviderEventID: event.ID,
		EventType:       string(event.Type),
		APIVersion:      event.APIVersion,
		Payload:         payload,
	})
	if err != nil {
		observeBillingWebhook(string(event.Type), "record_failed")
		h.d.Logger.ErrorContext(r.Context(), "org billing: record webhook receipt", "event_id", event.ID, "event_type", event.Type, "error", err)
		http.Error(w, "could not record webhook receipt", http.StatusInternalServerError)
		return
	}
	if !created && receipt.ProcessedAt.Valid {
		observeBillingWebhook(string(event.Type), "duplicate")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if err := h.processStripeWebhook(r.Context(), event); err != nil {
		observeBillingWebhook(string(event.Type), "process_failed")
		h.d.Logger.ErrorContext(r.Context(), "org billing: process webhook", "event_id", event.ID, "event_type", event.Type, "error", err)
		if _, markErr := orgbilling.MarkWebhookEventFailed(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, event.ID, err.Error()); markErr != nil {
			h.d.Logger.ErrorContext(r.Context(), "org billing: mark webhook failed", "event_id", event.ID, "error", markErr)
		}
		http.Error(w, "webhook processing failed", http.StatusInternalServerError)
		return
	}
	if _, err := orgbilling.MarkWebhookEventProcessed(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, event.ID); err != nil {
		observeBillingWebhook(string(event.Type), "finalize_failed")
		h.d.Logger.ErrorContext(r.Context(), "org billing: mark webhook processed", "event_id", event.ID, "error", err)
		http.Error(w, "could not finalize webhook receipt", http.StatusInternalServerError)
		return
	}
	observeBillingWebhook(string(event.Type), "processed")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func observeBillingWebhook(eventType, result string) {
	metrics.BillingWebhookEventsTotal.WithLabelValues(normalizeStripeBillingEventType(eventType), result).Inc()
}

func normalizeStripeBillingEventType(eventType string) string {
	switch eventType {
	case "checkout.session.completed",
		"customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted",
		"invoice.finalized",
		"invoice.payment_succeeded",
		"invoice.payment_failed",
		"invoice.voided",
		"invoice.marked_uncollectible",
		"charge.refunded",
		"unknown":
		return eventType
	default:
		return "other"
	}
}

func (h *Handlers) processStripeWebhook(ctx context.Context, event stripeapi.Event) error {
	switch string(event.Type) {
	case "checkout.session.completed":
		return h.applyStripeCheckoutCompleted(ctx, event)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		return h.applyStripeSubscriptionEvent(ctx, event)
	case "invoice.finalized", "invoice.payment_succeeded", "invoice.payment_failed", "invoice.voided", "invoice.marked_uncollectible":
		return h.applyStripeInvoiceEvent(ctx, event)
	case "charge.refunded":
		return h.applyStripeChargeRefunded(ctx, event)
	default:
		return nil
	}
}

func (h *Handlers) applyStripeCheckoutCompleted(ctx context.Context, event stripeapi.Event) error {
	var session stripeapi.CheckoutSession
	if err := unmarshalStripeEventObject(event, &session); err != nil {
		return err
	}
	customerID := stripeCustomerID(session.Customer)
	if customerID == "" {
		return errors.New("stripe checkout.session.completed missing customer")
	}
	principal, err := h.resolvePrincipalFromCheckout(ctx, &session, customerID)
	if err != nil {
		return err
	}
	h.recordWebhookSubject(ctx, event.ID, principal)
	if stale, err := h.checkStaleEvent(ctx, event, principal); err != nil || stale {
		return err
	}
	if _, err := orgbilling.SetStripeCustomerForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principal, customerID); err != nil {
		return err
	}
	h.touchLastEventAt(ctx, event, principal)
	return nil
}

// resolvePrincipalFromCheckout walks the resolution chain for a
// checkout.session.completed event. Order matches the spec:
//  1. metadata.shithub_subject_kind + shithub_subject_id (PRO04 path)
//  2. metadata.shithub_org_id (legacy SP03 path)
//  3. client_reference_id parsed as int (legacy SP03 path)
//  4. customer-id lookup against both tables
//
// Any path that yields a Principal returns immediately; the
// fall-through error covers events that can't be matched at all.
func (h *Handlers) resolvePrincipalFromCheckout(ctx context.Context, session *stripeapi.CheckoutSession, customerID string) (orgbilling.Principal, error) {
	if p, ok := stripePrincipalFromMetadata(session.Metadata); ok {
		return p, nil
	}
	if orgID := stripeOrgIDFromMetadata(session.Metadata); orgID != 0 {
		return orgbilling.PrincipalForOrg(orgID), nil
	}
	if id, err := strconv.ParseInt(strings.TrimSpace(session.ClientReferenceID), 10, 64); err == nil && id > 0 {
		// Legacy client_reference_id is org-only by convention.
		return orgbilling.PrincipalForOrg(id), nil
	}
	if customerID != "" {
		state, err := orgbilling.ResolvePrincipalByStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, customerID)
		if err == nil {
			return state.Principal, nil
		}
		if !errors.Is(err, orgbilling.ErrPrincipalNotFound) {
			return orgbilling.Principal{}, err
		}
	}
	return orgbilling.Principal{}, errors.New("stripe checkout.session.completed missing shithub subject metadata")
}

func (h *Handlers) applyStripeSubscriptionEvent(ctx context.Context, event stripeapi.Event) error {
	var sub stripeapi.Subscription
	if err := unmarshalStripeEventObject(event, &sub); err != nil {
		return err
	}
	principal, err := h.resolvePrincipalFromSubscription(ctx, &sub)
	if err != nil {
		// PRO08 D5: customer.subscription.deleted for a sub that's
		// not in our DB (no metadata match, no customer-id match, no
		// subscription-id match) — log + 200 no-op so Stripe stops
		// retrying. Loud-failure here only triggers infinite retries
		// against state we'll never own. Other event types still
		// surface the error so the operator notices misconfiguration.
		if string(event.Type) == "customer.subscription.deleted" {
			h.d.Logger.InfoContext(ctx, "org billing: subscription.deleted for unknown subject — no-op",
				"event_id", event.ID,
				"stripe_subscription_id", strings.TrimSpace(sub.ID),
				"error", err)
			return nil
		}
		return err
	}
	h.recordWebhookSubject(ctx, event.ID, principal)
	if stale, err := h.checkStaleEvent(ctx, event, principal); err != nil || stale {
		return err
	}
	// PRO08 D3: if the principal already has a different Stripe
	// subscription on file, refuse to overwrite it. A second sub
	// for the same customer (e.g., an operator created one manually
	// in the Stripe Dashboard) silently overwriting the first would
	// orphan the original — Stripe keeps billing both, shithub
	// tracks only the latest. Loud-fail so retries surface to the
	// operator. Skip the check on subscription.deleted: that path
	// reads the current sub id and clears state by design.
	if string(event.Type) != "customer.subscription.deleted" {
		if err := h.guardSubscriptionOverwrite(ctx, principal, &sub); err != nil {
			return err
		}
	}
	// Cross-kind price-id check: if the subscription's first item
	// price doesn't match the expected price for the resolved kind,
	// refuse to apply. A Pro price on an org subject (or Team on
	// user) means metadata was misconfigured in the Stripe Dashboard;
	// silently applying would corrupt the wrong table.
	if err := h.guardPriceKindMatch(principal.Kind, &sub); err != nil {
		return err
	}
	customerID := stripeCustomerID(sub.Customer)
	if customerID != "" {
		if _, err := orgbilling.SetStripeCustomerForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principal, customerID); err != nil {
			return err
		}
	}
	status, err := stripeSubscriptionStatus(sub.Status)
	if err != nil {
		return err
	}
	if status == orgbilling.SubscriptionStatusCanceled || string(event.Type) == "customer.subscription.deleted" {
		if _, err := orgbilling.MarkCanceledForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principal, event.ID); err != nil {
			return err
		}
		h.touchLastEventAt(ctx, event, principal)
		return nil
	}
	itemID := stripeSubscriptionItemID(sub.Items)
	licensedSeats := stripeSubscriptionItemQuantity(sub.Items)
	periodStart, periodEnd := stripeSubscriptionPeriod(sub.Items)
	if _, err := orgbilling.ApplySubscriptionSnapshotForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, orgbilling.PrincipalSubscriptionSnapshot{
		Principal:                principal,
		Status:                   status,
		StripeSubscriptionID:     strings.TrimSpace(sub.ID),
		StripeSubscriptionItemID: itemID,
		LicensedSeats:            licensedSeats,
		CurrentPeriodStart:       periodStart,
		CurrentPeriodEnd:         periodEnd,
		CancelAtPeriodEnd:        sub.CancelAtPeriodEnd,
		TrialEnd:                 unixTime(sub.TrialEnd),
		CanceledAt:               unixTime(sub.CanceledAt),
		LastWebhookEventID:       event.ID,
	}); err != nil {
		return err
	}
	h.touchLastEventAt(ctx, event, principal)
	return nil
}

// resolvePrincipalFromSubscription walks the same chain as the
// checkout resolver but starts from a subscription object.
func (h *Handlers) resolvePrincipalFromSubscription(ctx context.Context, sub *stripeapi.Subscription) (orgbilling.Principal, error) {
	if p, ok := stripePrincipalFromMetadata(sub.Metadata); ok {
		return p, nil
	}
	if orgID := stripeOrgIDFromMetadata(sub.Metadata); orgID != 0 {
		return orgbilling.PrincipalForOrg(orgID), nil
	}
	if customerID := stripeCustomerID(sub.Customer); customerID != "" {
		state, err := orgbilling.ResolvePrincipalByStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, customerID)
		if err == nil {
			return state.Principal, nil
		}
		if !errors.Is(err, orgbilling.ErrPrincipalNotFound) {
			return orgbilling.Principal{}, err
		}
	}
	if subID := strings.TrimSpace(sub.ID); subID != "" {
		state, err := orgbilling.ResolvePrincipalByStripeSubscription(ctx, orgbilling.Deps{Pool: h.d.Pool}, subID)
		if err == nil {
			return state.Principal, nil
		}
		if !errors.Is(err, orgbilling.ErrPrincipalNotFound) {
			return orgbilling.Principal{}, err
		}
	}
	return orgbilling.Principal{}, errors.New("stripe subscription does not map to a shithub subject")
}

// guardPriceKindMatch refuses to apply a subscription when the
// price-id on its first line item doesn't match the expected price
// for the resolved subject kind. Catches dashboard-side
// misconfiguration before it writes the wrong table.
//
// The check requires the handler to know which price-id is Pro and
// which is Team — that wiring lands via BillingPriceIDs(); a
// non-configured client (Pro disabled) skips the check rather than
// rejecting org events. PRO-disabled instances never see Pro
// events, so the org path is unaffected.
// guardSubscriptionOverwrite refuses to apply a subscription event
// when the principal already has a DIFFERENT subscription on file.
// Stripe can hold multiple subscriptions per customer; pointing the
// shithub side-state row at a second sub would orphan the first
// (Stripe keeps invoicing both; shithub tracks only the latest).
//
// The guard reads the current state for the resolved principal. If
// the persisted StripeSubscriptionID is empty or matches the
// incoming sub.ID, allow. Otherwise refuse — the operator must
// reconcile in the Stripe Dashboard before shithub flips.
//
// PRO08 D3.
func (h *Handlers) guardSubscriptionOverwrite(ctx context.Context, p orgbilling.Principal, sub *stripeapi.Subscription) error {
	if sub == nil {
		return nil
	}
	incoming := strings.TrimSpace(sub.ID)
	if incoming == "" {
		return nil
	}
	deps := orgbilling.Deps{Pool: h.d.Pool}
	switch p.Kind {
	case orgbilling.SubjectKindOrg:
		state, err := orgbilling.GetOrgBillingState(ctx, deps, p.ID)
		if err != nil {
			return err
		}
		current := strings.TrimSpace(state.StripeSubscriptionID.String)
		if state.StripeSubscriptionID.Valid && current != "" && current != incoming {
			return fmt.Errorf("stripe subscription: org %d already bound to subscription %q; refusing to overwrite with %q", p.ID, current, incoming)
		}
	case orgbilling.SubjectKindUser:
		state, err := orgbilling.GetUserBillingState(ctx, deps, p.ID)
		if err != nil {
			return err
		}
		current := strings.TrimSpace(state.StripeSubscriptionID.String)
		if state.StripeSubscriptionID.Valid && current != "" && current != incoming {
			return fmt.Errorf("stripe subscription: user %d already bound to subscription %q; refusing to overwrite with %q", p.ID, current, incoming)
		}
	}
	return nil
}

func (h *Handlers) guardPriceKindMatch(kind orgbilling.SubjectKind, sub *stripeapi.Subscription) error {
	teamPrice, proPrice := h.d.BillingPriceIDs()
	// PRO08 A1: when ANY price is configured we MUST be able to
	// inspect the event's price-id to enforce cross-kind separation.
	// A subscription event with empty Items can otherwise bypass the
	// guard entirely — a Pro-priced subscription with `subject_kind=org`
	// metadata would silently write Team to the org-side table. Refuse
	// the apply so Stripe retries (and the operator notices).
	if teamPrice != "" || proPrice != "" {
		if sub == nil || sub.Items == nil || len(sub.Items.Data) == 0 || sub.Items.Data[0] == nil || sub.Items.Data[0].Price == nil {
			id := ""
			if sub != nil {
				id = strings.TrimSpace(sub.ID)
			}
			return fmt.Errorf("stripe subscription %q: no line items in event — refusing apply (cross-kind price guard cannot run)", id)
		}
	} else if sub == nil || sub.Items == nil || len(sub.Items.Data) == 0 || sub.Items.Data[0] == nil || sub.Items.Data[0].Price == nil {
		// No prices configured AND no items — nothing to validate.
		// The instance has billing disabled or runs Pro-only / Team-
		// only without the other tier's price wired; let the apply
		// flow handle the rest.
		return nil
	}
	priceID := strings.TrimSpace(sub.Items.Data[0].Price.ID)
	switch kind {
	case orgbilling.SubjectKindOrg:
		if teamPrice != "" && priceID != "" && priceID != teamPrice {
			if priceID == proPrice {
				return fmt.Errorf("stripe subscription: Pro price %q applied to org subject — metadata likely misconfigured", priceID)
			}
			return fmt.Errorf("stripe subscription: price %q does not match expected team price %q for org subject", priceID, teamPrice)
		}
	case orgbilling.SubjectKindUser:
		if proPrice != "" && priceID != "" && priceID != proPrice {
			if priceID == teamPrice {
				return fmt.Errorf("stripe subscription: Team price %q applied to user subject — metadata likely misconfigured", priceID)
			}
			return fmt.Errorf("stripe subscription: price %q does not match expected pro price %q for user subject", priceID, proPrice)
		}
	}
	return nil
}

func (h *Handlers) applyStripeInvoiceEvent(ctx context.Context, event stripeapi.Event) error {
	var inv stripeapi.Invoice
	if err := unmarshalStripeEventObject(event, &inv); err != nil {
		return err
	}
	principalState, err := h.resolvePrincipalStateFromInvoice(ctx, &inv)
	if err != nil {
		return err
	}
	h.recordWebhookSubject(ctx, event.ID, principalState.Principal)
	if stale, err := h.checkStaleEvent(ctx, event, principalState.Principal); err != nil || stale {
		return err
	}
	status, err := stripeInvoiceStatus(inv.Status)
	if err != nil {
		return err
	}
	if _, err := orgbilling.UpsertInvoiceForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principalState.Principal, orgbilling.InvoiceSnapshot{
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
		if _, err := orgbilling.MarkPastDueForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principalState.Principal, graceUntil, event.ID); err != nil {
			return err
		}
	case "invoice.payment_succeeded":
		if principalState.SubscriptionStatus != orgbilling.SubscriptionStatusCanceled {
			if _, err := orgbilling.MarkPaymentSucceededForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, principalState.Principal, event.ID); err != nil {
				return err
			}
		}
	}
	h.touchLastEventAt(ctx, event, principalState.Principal)
	return nil
}

// applyStripeChargeRefunded handles PRO08 D2: a Stripe `charge.refunded`
// event flips the corresponding shithub invoice row to status='refunded'.
// Stripe itself leaves invoice.status='paid' after a refund; shithub
// maintains its own status for UI display.
//
// Refunds DO NOT automatically cancel the subscription. Operators
// who want to revoke Pro access in addition to issuing a refund must
// cancel the subscription via the Stripe Dashboard (which fires
// customer.subscription.deleted and is handled separately).
//
// A refund for an invoice shithub has never seen (the original
// invoice.payment_succeeded event never reached us, or it predates
// the polymorphic invoices table) logs a warning and returns nil so
// Stripe stops retrying — the operator must reconcile manually.
func (h *Handlers) applyStripeChargeRefunded(ctx context.Context, event stripeapi.Event) error {
	// The Stripe Go v85 Charge struct does not expose `invoice` as a
	// typed field; parse the raw event payload directly for the
	// invoice id (and customer id, used for fallback resolution).
	var charge struct {
		ID       string `json:"id"`
		Invoice  string `json:"invoice"`
		Customer string `json:"customer"`
	}
	if err := unmarshalStripeEventObject(event, &charge); err != nil {
		return err
	}
	invoiceID := strings.TrimSpace(charge.Invoice)
	if invoiceID == "" {
		// Standalone refund (no invoice linkage) — nothing to mark.
		h.d.Logger.InfoContext(ctx, "org billing: charge.refunded without invoice — skip",
			"event_id", event.ID, "charge_id", strings.TrimSpace(charge.ID))
		return nil
	}
	row, err := orgbilling.MarkInvoiceRefunded(ctx, orgbilling.Deps{Pool: h.d.Pool}, invoiceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.WarnContext(ctx, "org billing: charge.refunded for unknown invoice — operator must reconcile",
				"event_id", event.ID,
				"stripe_invoice_id", invoiceID)
			return nil
		}
		return err
	}
	// Stamp the receipt's subject from the persisted invoice row.
	// The polymorphic invoices schema guarantees subject_kind +
	// subject_id are NOT NULL on every row, so direct access is safe.
	principal := orgbilling.Principal{
		Kind: orgbilling.SubjectKind(row.SubjectKind),
		ID:   row.SubjectID,
	}
	h.recordWebhookSubject(ctx, event.ID, principal)
	h.touchLastEventAt(ctx, event, principal)
	return nil
}

// resolvePrincipalStateFromInvoice resolves Principal AND fetches
// the current billing state in one shot — the apply branch needs
// the SubscriptionStatus to decide whether to flip payment-
// succeeded transitions. Mirrors the legacy
// resolveOrgStateFromInvoice but returns a kind-tagged Principal.
func (h *Handlers) resolvePrincipalStateFromInvoice(ctx context.Context, inv *stripeapi.Invoice) (orgbilling.PrincipalState, error) {
	if customerID := stripeCustomerID(inv.Customer); customerID != "" {
		state, err := orgbilling.ResolvePrincipalByStripeCustomer(ctx, orgbilling.Deps{Pool: h.d.Pool}, customerID)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, orgbilling.ErrPrincipalNotFound) {
			return orgbilling.PrincipalState{}, err
		}
	}
	if subID := stripeInvoiceSubscriptionID(inv); subID != "" {
		state, err := orgbilling.ResolvePrincipalByStripeSubscription(ctx, orgbilling.Deps{Pool: h.d.Pool}, subID)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, orgbilling.ErrPrincipalNotFound) {
			return orgbilling.PrincipalState{}, err
		}
	}
	return orgbilling.PrincipalState{}, errors.New("stripe invoice does not map to a shithub subject")
}

// stripePrincipalFromMetadata reads the PRO04 subject metadata
// keys. Returns ok=false when either key is missing or malformed —
// the caller falls through to the legacy resolution chain.
func stripePrincipalFromMetadata(metadata map[string]string) (orgbilling.Principal, bool) {
	if len(metadata) == 0 {
		return orgbilling.Principal{}, false
	}
	kind := orgbilling.SubjectKind(strings.TrimSpace(metadata[stripebilling.MetadataSubjectKind]))
	if !kind.Valid() {
		return orgbilling.Principal{}, false
	}
	rawID := strings.TrimSpace(metadata[stripebilling.MetadataSubjectID])
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return orgbilling.Principal{}, false
	}
	return orgbilling.Principal{Kind: kind, ID: id}, true
}

// stripeOrgIDFromMetadata reads the legacy SP03 metadata key.
// PRO04 keeps it for backward compatibility — existing org
// subscriptions stamped before PRO04 deployed carry only this
// key. Resolvers try the PRO04 keys first, fall back to this.
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

func stripeSubscriptionItemID(items *stripeapi.SubscriptionItemList) string {
	if items == nil || len(items.Data) == 0 || items.Data[0] == nil {
		return ""
	}
	return strings.TrimSpace(items.Data[0].ID)
}

func stripeSubscriptionItemQuantity(items *stripeapi.SubscriptionItemList) int {
	if items == nil || len(items.Data) == 0 || items.Data[0] == nil {
		return 0
	}
	if items.Data[0].Quantity < 1 {
		return 0
	}
	return int(items.Data[0].Quantity)
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

// checkStaleEvent compares the incoming Stripe event's `created`
// timestamp to the principal's persisted last_event_at. Returns
// stale=true when the event is older than the last applied event,
// in which case the caller should return nil (the parent webhook
// handler logs MarkProcessed and Stripe stops retrying). Returns
// err only when the staleness query itself errored.
//
// PRO08 D4. Stripe doesn't guarantee delivery order across retries;
// without this guard a stale subscription.updated[active] arriving
// after a fresh subscription.updated[canceled] would re-activate the
// principal.
func (h *Handlers) checkStaleEvent(ctx context.Context, event stripeapi.Event, p orgbilling.Principal) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, nil
	}
	eventAt := unixTime(event.Created)
	if eventAt.IsZero() {
		// No timestamp on event — can't make a staleness judgment.
		return false, nil
	}
	stale, err := orgbilling.IsBillingEventStaleForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, p, eventAt)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "org billing: stale-event check failed",
			"event_id", event.ID, "principal", p.String(), "error", err)
		return false, nil
	}
	if stale {
		h.d.Logger.InfoContext(ctx, "org billing: dropping stale Stripe event",
			"event_id", event.ID,
			"event_type", event.Type,
			"event_created", eventAt,
			"principal", p.String())
	}
	return stale, nil
}

// touchLastEventAt updates the principal's last_event_at after a
// successful apply. Logs and continues on error — this is auxiliary
// to the load-bearing state mutation.
func (h *Handlers) touchLastEventAt(ctx context.Context, event stripeapi.Event, p orgbilling.Principal) {
	if err := p.Validate(); err != nil {
		return
	}
	eventAt := unixTime(event.Created)
	if eventAt.IsZero() {
		return
	}
	if err := orgbilling.TouchBillingLastEventAtForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, p, eventAt); err != nil {
		h.d.Logger.WarnContext(ctx, "org billing: touch last_event_at failed",
			"event_id", event.ID, "principal", p.String(), "error", err)
	}
}

// recordWebhookSubject persists the resolved principal on the receipt
// row so failed events keep their audit trail. Logs and continues on
// error — the subject is auxiliary; the state-mutation path is the
// load-bearing thing. A zero principal (invalid kind / ID) is treated
// as a programmer error and silently dropped: the both-or-neither
// CHECK constraint on the receipt table would reject the write.
func (h *Handlers) recordWebhookSubject(ctx context.Context, eventID string, p orgbilling.Principal) {
	if err := p.Validate(); err != nil {
		h.d.Logger.WarnContext(ctx, "org billing: webhook subject record skipped — invalid principal",
			"event_id", eventID, "error", err)
		return
	}
	if err := orgbilling.SetWebhookEventSubjectForPrincipal(ctx, orgbilling.Deps{Pool: h.d.Pool}, eventID, p); err != nil {
		h.d.Logger.WarnContext(ctx, "org billing: webhook subject record failed",
			"event_id", eventID, "principal", p.String(), "error", err)
	}
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
