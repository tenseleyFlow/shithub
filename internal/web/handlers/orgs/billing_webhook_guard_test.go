// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// PRO08 cross-kind price guard tests.
//
// These lock guardPriceKindMatch behavior:
//   - A1: empty items refused when prices are configured (else the guard
//     can't run and a misrouted Pro-priced sub silently writes Team).
//   - Pro price on org subject ⇒ refused, no state mutation.
//   - Team price on user subject ⇒ refused.
//   - Receipt row is marked failed with the guard's error message.

const (
	testTeamPriceID = "price_team_test"
	testProPriceID  = "price_pro_test"
)

func TestBillingWebhookGuardRefusesEmptyItemsWhenPricesConfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_empty_items",
		"customer": "cus_empty_items",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		// items field omitted entirely — Stripe rarely sends this but
		// a malformed event MUST loud-fail rather than bypass the guard.
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_empty_items",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_empty_items")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("empty-items webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	// State must not have flipped.
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != orgbilling.PlanFree {
		t.Fatalf("guarded webhook should leave org Free, got %s", state.Plan)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_empty_items")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if receipt.ProcessError == "" || !strings.Contains(receipt.ProcessError, "no line items") {
		t.Fatalf("expected guard failure receipt, got process_error=%q", receipt.ProcessError)
	}
}

func TestBillingWebhookGuardRefusesProPriceOnOrgSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_pro_on_org",
		"customer": "cus_pro_on_org",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_pro_on_org",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testProPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_pro_on_org",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_pro_on_org")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("misroute status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != orgbilling.PlanFree {
		t.Fatalf("misroute should leave org Free, got %s", state.Plan)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_pro_on_org")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if !strings.Contains(receipt.ProcessError, "Pro price") {
		t.Fatalf("expected Pro-price-on-org error, got %q", receipt.ProcessError)
	}
}

func TestBillingWebhookGuardRefusesTeamPriceOnUserSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_team_on_user",
		"customer": "cus_team_on_user",
		"status":   "active",
		"metadata": map[string]string{
			stripebilling.MetadataSubjectKind: "user",
			stripebilling.MetadataSubjectID:   strconv.FormatInt(ownerID, 10),
		},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_team_on_user",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testTeamPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_team_on_user",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_team_on_user")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("misroute status=%d body=%s", resp.Code, resp.Body.String())
	}
	userState, err := orgbilling.GetUserBillingState(ctx, orgbilling.Deps{Pool: pool}, ownerID)
	if err != nil {
		t.Fatalf("GetUserBillingState: %v", err)
	}
	if userState.Plan != orgbilling.UserPlanFree {
		t.Fatalf("misroute should leave user Free, got %s", userState.Plan)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_team_on_user")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if !strings.Contains(receipt.ProcessError, "Team price") {
		t.Fatalf("expected Team-price-on-user error, got %q", receipt.ProcessError)
	}
}

// TestBillingWebhookDropsStaleEvent locks PRO08 D4: a Stripe event
// with `created` older than the persisted last_event_at must NOT
// regress state. Pre-PRO08 a reverse-ordered retry could re-activate
// a canceled subscription. The handler returns 200 (Stripe stops
// retrying THIS delivery) and leaves state alone.
func TestBillingWebhookDropsStaleEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	// Establish a fresh canceled state via direct apply + touch.
	if _, err := orgbilling.MarkCanceledForPrincipal(ctx, orgbilling.Deps{Pool: pool}, orgbilling.PrincipalForOrg(orgID), "evt_canceled"); err != nil {
		t.Fatalf("MarkCanceled: %v", err)
	}
	freshTime := time.Now().UTC()
	if err := orgbilling.TouchBillingLastEventAtForPrincipal(ctx, orgbilling.Deps{Pool: pool}, orgbilling.PrincipalForOrg(orgID), freshTime); err != nil {
		t.Fatalf("touch fresh: %v", err)
	}

	// A stale (older) subscription.updated[active] arrives. event.Created
	// is 1 hour BEFORE the persisted last_event_at.
	staleCreated := freshTime.Add(-1 * time.Hour).Unix()
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_stale_active",
		"customer": "cus_stale",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_stale",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testTeamPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:      "evt_stale_active",
				Type:    stripeapi.EventType("customer.subscription.updated"),
				Created: staleCreated,
				Data:    &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_stale_active")
	if resp.Code != http.StatusOK {
		t.Fatalf("stale event status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if state.Plan != orgbilling.PlanFree {
		t.Fatalf("stale event corrupted state: plan=%s want free", state.Plan)
	}
	if state.SubscriptionStatus != orgbilling.SubscriptionStatusCanceled {
		t.Fatalf("stale event corrupted status: got %s want canceled", state.SubscriptionStatus)
	}
}

// TestBillingWebhookGuardRefusesSecondSubscriptionForSameCustomer locks
// PRO08 D3: when the principal already has a Stripe subscription on
// file, a webhook event referencing a DIFFERENT subscription must be
// refused. Pre-PRO08 it silently overwrote, orphaning the first sub.
func TestBillingWebhookGuardRefusesSecondSubscriptionForSameCustomer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	// Seed the org with an existing subscription via direct apply.
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                orgID,
		Plan:                 orgbilling.PlanTeam,
		Status:               orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID: "sub_FIRST",
		LastWebhookEventID:   "evt_seed",
	}); err != nil {
		t.Fatalf("seed first sub: %v", err)
	}

	// Webhook now arrives for a SECOND subscription on the same org.
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_SECOND",
		"customer": "cus_overlap",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_second",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testTeamPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_second_sub",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_second_sub")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (refuse + Stripe retry), got %d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.StripeSubscriptionID.String != "sub_FIRST" {
		t.Fatalf("original sub overwritten: got %q want sub_FIRST", state.StripeSubscriptionID.String)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_second_sub")
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if !strings.Contains(receipt.ProcessError, "already bound to subscription") {
		t.Errorf("expected overwrite-refusal error, got %q", receipt.ProcessError)
	}
}

// TestBillingWebhookGuardAllowsSameSubscriptionUpdate confirms the
// guard doesn't false-positive on the common case: subscription.updated
// for the SAME subscription id (e.g., status flip from active →
// past_due).
func TestBillingWebhookGuardAllowsSameSubscriptionUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                orgID,
		Plan:                 orgbilling.PlanTeam,
		Status:               orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID: "sub_same",
		LastWebhookEventID:   "evt_seed_same",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"id":       "sub_same",
		"customer": "cus_same",
		"status":   "past_due",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_same",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testTeamPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_same_sub",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_same_sub")
	if resp.Code != http.StatusOK {
		t.Fatalf("same-sub status flip should succeed, got %d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if state.SubscriptionStatus != orgbilling.SubscriptionStatusPastDue {
		t.Fatalf("expected past_due, got %s", state.SubscriptionStatus)
	}
}

// TestBillingWebhookGuardAllowsCorrectKindPriceMatch is the happy-path
// sanity check: the right price for the right kind passes the guard.
func TestBillingWebhookGuardAllowsCorrectKindPriceMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_team_on_org",
		"customer": "cus_team_on_org",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_team_on_org",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
			"price":                map[string]string{"id": testTeamPriceID},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:   "evt_team_on_org",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMuxWithPrices(t, pool, ownerID, fake, testTeamPriceID, testProPriceID)
	resp := postBillingWebhook(t, mux, "evt_team_on_org")
	if resp.Code != http.StatusOK {
		t.Fatalf("happy-path status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != orgbilling.PlanTeam {
		t.Fatalf("happy-path should apply Team plan, got %s", state.Plan)
	}
}

