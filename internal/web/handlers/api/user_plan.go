// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// userPlanFeatures lists every Feature* registered as applying to user
// kind. The response surfaces a per-feature `allowed` flag for each one
// so a CLI client can render its own locked UI from a single read.
//
// Keep this in sync with featureKinds in
// internal/entitlements/entitlements.go — every new Feature* added by a
// PRO-EXT01-NN sprint should land here so third-party tools see the
// new gate. PRO-EXT01-17 plans a per-feature Prometheus counter that
// will also iterate this list.
var userPlanFeatures = []entitlements.Feature{
	entitlements.FeatureProfilePinsBeyondFree,
	entitlements.FeatureRequiredReviewers,
	entitlements.FeatureAdvancedBranchProtection,
	entitlements.FeatureCodeOwnersReview,
	entitlements.FeatureProfileVanity,
	entitlements.FeatureUsernameReservations,
}

// userPlanResponse is the JSON shape for GET /api/v1/user/plan.
//
// The shape is identical for Free and Pro accounts; only the field
// values differ. A Free response substitutes plan="free", status="none",
// allowed=false on every feature, and limits.profile_pins=6 so the
// client renders a consistent locked UI without checking plan first.
type userPlanResponse struct {
	Plan              string                          `json:"plan"`
	Status            string                          `json:"status"`
	CurrentPeriodEnd  *string                         `json:"current_period_end"`
	CancelAtPeriodEnd bool                            `json:"cancel_at_period_end"`
	GraceUntil        *string                         `json:"grace_until"`
	Features          map[string]userPlanFeatureEntry `json:"features"`
	Limits            map[string]int64                `json:"limits"`
}

type userPlanFeatureEntry struct {
	Allowed bool `json:"allowed"`
}

// mountUserPlan registers GET /api/v1/user/plan. PAT-authed; user:read
// scope. Existence-leak-safe: every authenticated user gets a response
// (no 404 for users without Stripe records — the user_billing_states
// trigger seeds a Free row at signup).
func (h *Handlers) mountUserPlan(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/plan", h.userPlan)
	})
}

func (h *Handlers) userPlan(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	set, err := entitlements.ForPrincipal(r.Context(), entitlements.Deps{Pool: h.d.Pool}, billing.PrincipalForUser(auth.UserID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: user plan entitlements", "user_id", auth.UserID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "could not load plan")
		return
	}
	writeJSON(w, http.StatusOK, buildUserPlanResponse(set))
}

func buildUserPlanResponse(set entitlements.Set) userPlanResponse {
	state := set.UserState
	features := make(map[string]userPlanFeatureEntry, len(userPlanFeatures))
	for _, feature := range userPlanFeatures {
		decision := set.CanUse(feature)
		features[string(feature)] = userPlanFeatureEntry{Allowed: decision.Allowed}
	}
	pinCap := entitlements.FreeProfilePinsCap
	if features[string(entitlements.FeatureProfilePinsBeyondFree)].Allowed {
		pinCap = entitlements.ProProfilePinsCap
	}
	resp := userPlanResponse{
		Plan:              string(state.Plan),
		Status:            string(state.SubscriptionStatus),
		CancelAtPeriodEnd: state.CancelAtPeriodEnd,
		Features:          features,
		Limits:            map[string]int64{"profile_pins": pinCap},
	}
	if billing.IsUserPlanUnset(state.Plan) {
		resp.Plan = "free"
	}
	if state.SubscriptionStatus == "" {
		resp.Status = "none"
	}
	if state.CurrentPeriodEnd.Valid {
		s := state.CurrentPeriodEnd.Time.UTC().Format(time.RFC3339)
		resp.CurrentPeriodEnd = &s
	}
	if state.GraceUntil.Valid {
		s := state.GraceUntil.Time.UTC().Format(time.RFC3339)
		resp.GraceUntil = &s
	}
	return resp
}
