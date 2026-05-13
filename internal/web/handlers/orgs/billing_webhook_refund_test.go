// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// PRO08 D2 refund tests.
//
// charge.refunded events arrive out-of-band after a Stripe-side refund.
// The invoice itself stays paid in Stripe; shithub flips its own row
// to status='refunded' for UI surfacing.

func TestBillingWebhookChargeRefundedMarksInvoiceRefunded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	// Seed a paid invoice via the polymorphic upsert.
	if _, err := orgbilling.UpsertInvoiceForPrincipal(ctx, orgbilling.Deps{Pool: pool}, orgbilling.PrincipalForOrg(orgID), orgbilling.InvoiceSnapshot{
		StripeInvoiceID:  "in_paid_1",
		StripeCustomerID: "cus_refund",
		Status:           orgbilling.InvoiceStatusPaid,
		Number:           "INV-001",
		Currency:         "usd",
		AmountDueCents:   400,
		AmountPaidCents:  400,
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"id":       "ch_refund_1",
		"invoice":  "in_paid_1",
		"customer": "cus_refund",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_refund_1",
				Type: stripeapi.EventType("charge.refunded"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)
	resp := postBillingWebhook(t, mux, "evt_refund_1")
	if resp.Code != http.StatusOK {
		t.Fatalf("charge.refunded status=%d body=%s", resp.Code, resp.Body.String())
	}

	// Verify the invoice was flipped to refunded.
	var status billingdb.BillingInvoiceStatus
	var refundedAtValid bool
	if err := pool.QueryRow(ctx,
		`SELECT status, refunded_at IS NOT NULL FROM billing_invoices WHERE stripe_invoice_id = 'in_paid_1'`,
	).Scan(&status, &refundedAtValid); err != nil {
		t.Fatalf("query invoice: %v", err)
	}
	if status != billingdb.BillingInvoiceStatusRefunded {
		t.Fatalf("invoice status: got %q, want refunded", status)
	}
	if !refundedAtValid {
		t.Fatalf("refunded_at not set")
	}

	// Verify the receipt records the subject.
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_refund_1")
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if !receipt.SubjectKind.Valid || receipt.SubjectKind.BillingSubjectKind != billingdb.BillingSubjectKindOrg {
		t.Errorf("receipt subject_kind: got %+v, want org", receipt.SubjectKind)
	}
	if !receipt.SubjectID.Valid || receipt.SubjectID.Int64 != orgID {
		t.Errorf("receipt subject_id: got %+v, want %d", receipt.SubjectID, orgID)
	}
}

// TestBillingWebhookChargeRefundedForUnknownInvoiceIsNoOp locks PRO08
// D2's degraded-path behavior: a refund for an invoice we've never
// seen logs a warning and returns 200 so Stripe stops retrying. The
// operator reconciles manually.
func TestBillingWebhookChargeRefundedForUnknownInvoiceIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	_ = insertOrgAvatarOrg(t, pool, ownerID, "acme")

	raw, err := json.Marshal(map[string]any{
		"id":       "ch_ghost",
		"invoice":  "in_never_seen",
		"customer": "cus_ghost",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_refund_ghost",
				Type: stripeapi.EventType("charge.refunded"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)
	resp := postBillingWebhook(t, mux, "evt_refund_ghost")
	if resp.Code != http.StatusOK {
		t.Fatalf("ghost refund status=%d body=%s (expected 200 no-op)", resp.Code, resp.Body.String())
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_refund_ghost")
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if !receipt.ProcessedAt.Valid {
		t.Fatalf("ghost-refund receipt not marked processed: %+v", receipt)
	}
}

// TestBillingWebhookChargeRefundedWithoutInvoiceIsNoOp locks the
// standalone-refund path — a refund not linked to any invoice (e.g.,
// a one-off charge refund) is a no-op for the polymorphic-invoices
// surface.
func TestBillingWebhookChargeRefundedWithoutInvoiceIsNoOp(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	_ = insertOrgAvatarOrg(t, pool, ownerID, "acme")

	raw, err := json.Marshal(map[string]any{
		"id":       "ch_standalone",
		"invoice":  "", // explicit empty
		"customer": "cus_standalone",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_refund_standalone",
				Type: stripeapi.EventType("charge.refunded"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)
	resp := postBillingWebhook(t, mux, "evt_refund_standalone")
	if resp.Code != http.StatusOK {
		t.Fatalf("standalone-refund status=%d body=%s", resp.Code, resp.Body.String())
	}
}
