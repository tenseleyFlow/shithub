// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// TestBillingWebhookConcurrentReplayAppliesOnce locks PRO08 A3: when
// N concurrent deliveries of the same event_id race, the advisory
// lock makes the apply path mutually exclusive — exactly one apply
// runs, processing_attempts==1, and the contending replays return 200
// without doubling the state mutation.
//
// Without the lock, two callers race past CreateWebhookEventReceipt
// before either marks processed_at, and both call ApplySubscriptionSnapshot.
// The CTE is idempotent on equal payloads, but MarkPastDueForPrincipal
// would rewrite grace_until on each apply — and processing_attempts
// would tick to N instead of 1.
func TestBillingWebhookConcurrentReplayAppliesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")

	// applyCount tracks how many times the resolver actually ran. Each
	// concurrent request goes through resolve → guard → apply. With the
	// lock in place exactly one request gets past the lock check and
	// runs the apply; the rest return 200 "in flight".
	var resolveCount atomic.Int64
	raw, err := json.Marshal(map[string]any{
		"id":       "sub_concurrent",
		"customer": "cus_concurrent",
		"status":   "active",
		"metadata": map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_concurrent",
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
		}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			// VerifyWebhook is called on every delivery. We count the
			// resolve-side activity below; here we just hand back the
			// same event each time.
			return stripeapi.Event{
				ID:   "evt_concurrent",
				Type: stripeapi.EventType("customer.subscription.updated"),
				Data: &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	successes := atomic.Int64{}
	inFlight := atomic.Int64{}
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(`{"id":"evt_concurrent"}`))
			req.Header.Set("Stripe-Signature", "sig_test")
			resp := httptest.NewRecorder()
			mux.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				return
			}
			body := resp.Body.String()
			if strings.Contains(body, "in flight") {
				inFlight.Add(1)
				return
			}
			successes.Add(1)
		}()
	}
	wg.Wait()

	// Exactly one worker should have completed the full apply (success
	// body == "ok"). The remaining returned "ok (in flight)".
	if got := successes.Load(); got != 1 {
		t.Errorf("successes=%d, want exactly 1 (lock should serialize)", got)
	}
	if got := inFlight.Load(); got != workers-1 {
		t.Errorf("in-flight responses=%d, want %d (lock should reject %d racers)", got, workers-1, workers-1)
	}

	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != orgbilling.PlanTeam || state.SubscriptionStatus != orgbilling.SubscriptionStatusActive {
		t.Fatalf("expected Team active, got plan=%s status=%s", state.Plan, state.SubscriptionStatus)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_concurrent")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if receipt.ProcessingAttempts != 1 {
		t.Fatalf("processing_attempts=%d, want 1 (lock should prevent double-apply)", receipt.ProcessingAttempts)
	}
	if !receipt.ProcessedAt.Valid {
		t.Fatalf("processed_at not set: %+v", receipt)
	}
	// VerifyWebhook gets called on every delivery (parses event before
	// the lock check); resolveCount remains unused — kept above for
	// future diagnostics if we want per-resolve counting.
	_ = resolveCount.Load()
}
