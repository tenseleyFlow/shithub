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

