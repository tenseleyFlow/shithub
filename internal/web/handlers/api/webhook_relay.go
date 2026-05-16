// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

// PRO-EXT01-13a: webhook relay receiver. Mounted at
// POST /webhook-relay/{token}. The token in the URL is the only
// authentication — no PAT, no session cookie. Upstream services paste
// the URL into their webhook config and shithub fans the body out to
// the user's configured destinations.
//
// Security model — these are the load-bearing defenses:
//
//   - Token is SHA-256 hashed in the DB. Receiver looks up by hash;
//     mismatches collapse to 404 regardless of why (malformed,
//     unknown, deleted) so an attacker can't probe the keyspace.
//   - Body cap at MaxInboundBody (1 MiB).
//   - Per-token rate limit: a noisy upstream can't weaponize the
//     relay as an amplifier. Tunable via the policy below.
//   - Disabled relays return 410 — the user knows their token is
//     "still alive" but the relay won't deliver until re-enabled.
//   - Entitlement gate: Free users return 403 under enforce; under
//     report-only, the gate logs `entitlements.report_only_deny`
//     with surface="webhook-relay" so SREs see relay traffic shape.
//   - Outbound delivery (in the worker) re-validates the destination
//     URL via SSRF defense — DNS rebind defense.
const webhookRelayMaxBody = webhookrelay.MaxInboundBody

// webhookRelayRateLimit caps per-token inbound rate. 600/minute is a
// generous default — busy CI sourced from GitHub Actions sends well
// under this. Operators can override the scope counter via the
// existing ratelimit overrides table.
var webhookRelayRateLimit = ratelimit.Policy{
	Scope:  "webhook-relay:ingest",
	Max:    600,
	Window: time.Minute,
}

// mountWebhookRelay registers the receiver. Lives at top-level so
// upstream webhook configs paste a single URL — no /api/v1 prefix.
//
// Mount it from the api Handlers' Mount() outside the PAT-auth group
// (no token, no scope check). Body cap is enforced inline because
// the route lives outside the api-body-cap group.
func (h *Handlers) mountWebhookRelay(r chi.Router) {
	if h.d.RateLimiter != nil {
		r.With(h.d.RateLimiter.Middleware(webhookRelayRateLimit, webhookRelayTokenKey)).
			Post("/webhook-relay/{token}", h.webhookRelayReceive)
	} else {
		r.Post("/webhook-relay/{token}", h.webhookRelayReceive)
	}
}

// webhookRelayTokenKey extracts the {token} URL param as the rate-
// limit key. Returning empty string makes the limiter no-op for
// requests missing the param — chi would have routed elsewhere
// already, so this is defensive.
func webhookRelayTokenKey(r *http.Request) string {
	tok := chi.URLParam(r, "token")
	if tok == "" {
		return ""
	}
	return "relay:" + tok
}

func (h *Handlers) webhookRelayReceive(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	if h.d.SecretBox == nil {
		// Operator hasn't configured the AEAD box — refuse rather than
		// loop on relays whose HMAC secrets we can't decrypt.
		h.d.Logger.WarnContext(r.Context(), "webhookrelay: no SecretBox configured; refusing inbound")
		writeAPIError(w, http.StatusServiceUnavailable, "relay disabled")
		return
	}

	// Cap inline — this route isn't under the api-wide body cap.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhookRelayMaxBody))
	if err != nil {
		// MaxBytesReader returns a *http.MaxBytesError on overflow;
		// distinguishing helps the caller fix their config rather
		// than think there's a transient network issue.
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "read body")
		return
	}

	deps := webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}
	relay, _, _, lookupErr := deps.LookupByToken(r.Context(), token)
	switch {
	case errors.Is(lookupErr, webhookrelay.ErrMalformed):
		// Collapse malformed → 404 so an attacker can't differentiate
		// "bad shape" from "unknown" while probing.
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(lookupErr, webhookrelay.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(lookupErr, webhookrelay.ErrDisabled):
		// 410 Gone — distinct from 404 so operators see "exists but
		// turned off" in their logs vs "never existed".
		writeAPIError(w, http.StatusGone, "relay disabled")
		return
	case lookupErr != nil:
		h.d.Logger.ErrorContext(r.Context(), "webhookrelay: lookup",
			"error", lookupErr)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	if !h.webhookRelayGate(r.Context(), w, relay.UserID) {
		return
	}

	res, err := deps.Ingest(r.Context(), h.d.Logger, relay, body)
	if err != nil {
		// Ingest returns ErrDisabled if the relay was disabled
		// between LookupByToken and Ingest — race surface, but
		// possible. Re-collapse to 410.
		if errors.Is(err, webhookrelay.ErrDisabled) {
			writeAPIError(w, http.StatusGone, "relay disabled")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "webhookrelay: ingest",
			"relay_id", relay.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "ingest failed")
		return
	}

	w.Header().Set("X-Shithub-Relay-Request", res.RequestID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"request_id":     res.RequestID,
		"delivery_count": res.DeliveryRows,
	})
}

// webhookRelayGate runs the FeatureWebhookRelay decision. On disallow
// it always emits the report_only_deny log line (surface tag lets
// SREs filter relay-only); on enforce-on it returns 403 too. Returns
// true if the caller should proceed to ingest.
func (h *Handlers) webhookRelayGate(ctx context.Context, w http.ResponseWriter, userID int64) bool {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureWebhookRelay)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "webhookrelay: entitlement check",
			"user_id", userID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "entitlement check failed")
		return false
	}
	if !decision.Allowed && h.d.Logger != nil {
		mode := "report_only"
		if h.d.BillingEnforce.UserWebhookRelay {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", orgbilling.PrincipalForUser(userID).String(),
			"principal_kind", string(orgbilling.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureWebhookRelay),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "webhook-relay")
	}
	if !decision.Allowed && h.d.BillingEnforce.UserWebhookRelay {
		writeAPIError(w, http.StatusForbidden,
			"webhook relay requires a Pro subscription")
		return false
	}
	return true
}
