// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

// PRO-EXT01-13c: CRUD REST for /api/v1/user/webhook-relays/*.
//
// Scope split matches PRO-EXT_SR-06: user:read on GETs, user:write on
// POST + DELETE + disable. The shape mirrors the personal Actions
// secrets endpoints — POST returns the raw token exactly once; LIST
// returns metadata only (no token, no HMAC).

// mountUserWebhookRelays registers the CRUD routes. Called from
// Mount() inside the PAT-auth + body-cap group.
func (h *Handlers) mountUserWebhookRelays(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/webhook-relays", h.userWebhookRelaysList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Post("/api/v1/user/webhook-relays", h.userWebhookRelaysCreate)
		r.Delete("/api/v1/user/webhook-relays/{id}", h.userWebhookRelaysDelete)
		r.Post("/api/v1/user/webhook-relays/{id}/disable", h.userWebhookRelaysDisable)
	})
}

// relayResponse is the public list/get shape. Token is NEVER included;
// only the prefix is exposed for UI display.
type relayResponse struct {
	ID           int64                      `json:"id"`
	Name         string                     `json:"name"`
	TokenPrefix  string                     `json:"token_prefix"`
	Destinations []webhookrelay.Destination `json:"destinations"`
	Disabled     bool                       `json:"disabled"`
}

// relayCreateResponse extends relayResponse with the one-shot raw
// token. Callers MUST persist this on the receiving end — it is not
// recoverable after the response is consumed.
type relayCreateResponse struct {
	relayResponse
	Token       string `json:"token"`
	ReceiverURL string `json:"receiver_url"`
}

type relayCreateRequest struct {
	Name         string                     `json:"name"`
	Destinations []webhookrelay.Destination `json:"destinations"`
}

func presentRelay(r webhookrelay.Relay) relayResponse {
	return relayResponse{
		ID:           r.ID,
		Name:         r.Name,
		TokenPrefix:  r.TokenPrefix,
		Destinations: r.Destinations,
		Disabled:     r.Disabled,
	}
}

func (h *Handlers) userWebhookRelaysList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	rows, err := webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, SSRF: h.webhookSSRFConfig()}.
		ListForUser(r.Context(), userID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list webhook relays", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]relayResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentRelay(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) userWebhookRelaysCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "relay disabled")
		return
	}
	if !h.userWebhookRelayGate(r.Context(), w, userID, "create") {
		return
	}
	var body relayCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	hmacSecret, err := newHMACSecret()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: hmac secret gen", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret gen failed")
		return
	}
	deps := webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, SSRF: h.webhookSSRFConfig()}
	res, err := deps.Create(r.Context(), webhookrelay.CreateInput{
		UserID:       userID,
		Name:         body.Name,
		HMACSecret:   hmacSecret,
		Destinations: body.Destinations,
	})
	if err != nil {
		switch {
		case errors.Is(err, webhookrelay.ErrEmptyName):
			writeAPIError(w, http.StatusBadRequest, "name is required")
		case errors.Is(err, webhookrelay.ErrTooManyDestinations):
			writeAPIError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, webhookrelay.ErrInvalidDestination):
			// PRO-EXT_SR2-10 (audit H1): surface the wrapped reason
			// so CLI clients can identify which destination index
			// failed and why.
			writeAPIError(w, http.StatusBadRequest, err.Error())
		default:
			h.d.Logger.ErrorContext(r.Context(), "api: create webhook relay", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "create failed")
		}
		return
	}
	resp := relayCreateResponse{
		relayResponse: presentRelay(res.Relay),
		Token:         res.RawToken,
		ReceiverURL:   h.d.BaseURL + "/webhook-relay/" + res.RawToken,
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handlers) userWebhookRelaysDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	id, ok := parseInt64Param(w, r, "id")
	if !ok {
		return
	}
	if !h.assertRelayOwner(r.Context(), w, id, userID) {
		return
	}
	if err := (webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, SSRF: h.webhookSSRFConfig()}).
		Delete(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete webhook relay", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) userWebhookRelaysDisable(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	id, ok := parseInt64Param(w, r, "id")
	if !ok {
		return
	}
	if !h.assertRelayOwner(r.Context(), w, id, userID) {
		return
	}
	if err := (webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, SSRF: h.webhookSSRFConfig()}).
		Disable(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: disable webhook relay", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "disable failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// assertRelayOwner fetches the relay + verifies ownership. Returns
// true to proceed. 404 collapses missing + not-owner so attackers
// can't probe IDs.
func (h *Handlers) assertRelayOwner(ctx context.Context, w http.ResponseWriter, id, userID int64) bool {
	relay, _, err := (webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, SSRF: h.webhookSSRFConfig()}).
		GetByID(ctx, id)
	if err != nil || relay.UserID != userID {
		writeAPIError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// userWebhookRelayGate runs the FeatureWebhookRelay entitlement check
// for the CRUD paths. Same shape as the receiver gate but on a
// different surface; logs surface=webhook-relay-rest for SREs to
// distinguish CRUD denies from inbound denies.
func (h *Handlers) userWebhookRelayGate(ctx context.Context, w http.ResponseWriter, userID int64, action string) bool {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureWebhookRelay)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "api: webhook-relay entitlement check", "error", err)
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
			"surface", "webhook-relay-rest",
			"action", action)
	}
	if !decision.Allowed && h.d.BillingEnforce.UserWebhookRelay {
		writeAPIError(w, http.StatusForbidden,
			"webhook relay requires a Pro subscription")
		return false
	}
	return true
}

// newHMACSecret returns 32 random bytes for the per-relay shared
// secret. The secret is stored AEAD-encrypted; this raw value is
// never returned to the API caller (PR 13c keeps the secret server-
// side; users sign outbound destinations with the per-delivery
// signature header).
func newHMACSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// parseInt64Param extracts a chi URL int64 param. Writes 404 on
// non-integer input (an attacker fuzzing path segments shouldn't
// distinguish "bad shape" from "unknown id").
func parseInt64Param(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}
