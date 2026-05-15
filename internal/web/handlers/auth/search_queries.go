// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-08a — saved code-search queries CRUD.
//
// Gate posture mirrors PRO-EXT01-05 username reservations: Free users
// reach the settings page (so the locked-UI affordance is discoverable)
// and the form renders disabled. Direct POSTs from a crafted client
// hit the orchestrator-level check; report-only logs the would-deny
// and lets the insert land; enforce mode refuses with the upgrade
// banner. The regex `kind` is itself a Pro feature; this PR accepts
// only 'plain' kind to keep the data clean — 08b extends the form
// to accept 'regex' once the regex query path lands.
//
// The existing global code search at /search/code (internal/search)
// remains free for everyone; this sprint adds *additional* Pro
// capabilities on top.

const (
	searchQueryMaxNameChars      = 80
	searchQueryMaxQueryTextChars = 1000
	searchQueryMaxScopeChars     = 200
)

func (h *Handlers) settingsSearchQueriesForm(w http.ResponseWriter, r *http.Request) {
	h.renderSearchQueriesForm(w, r, "", "")
}

func (h *Handlers) settingsSearchQueryCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	queryText := strings.TrimSpace(r.PostFormValue("query_text"))
	scope := strings.TrimSpace(r.PostFormValue("scope_filter"))

	if msg := validateSearchQuery(name, queryText, scope); msg != "" {
		h.renderSearchQueriesForm(w, r, msg, "")
		return
	}

	allowed, decision, err := h.advancedCodeSearchAllowed(r.Context(), user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/search-queries: entitlement check", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserAdvancedCodeSearch {
		banner := decision.PrincipalUpgradeBanner("Saved code-search queries", billing.PrincipalForUser(user.ID), "")
		h.renderSearchQueriesForm(w, r, banner.Message, "")
		return
	}

	if _, err := h.q.InsertCodeSearchSavedQuery(r.Context(), h.d.Pool, usersdb.InsertCodeSearchSavedQueryParams{
		UserID:      user.ID,
		Name:        name,
		QueryText:   queryText,
		Kind:        usersdb.CodeSearchQueryKindPlain,
		ScopeFilter: scope,
	}); err != nil {
		if isUniqueViolation(err) {
			h.renderSearchQueriesForm(w, r, "You already have a saved query with that name.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "settings/search-queries: insert", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSearchQueriesForm(w, r, "", "Saved query added.")
}

func (h *Handlers) settingsSearchQueryUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSearchQueriesForm(w, r, "Invalid saved-query id.", "")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	queryText := strings.TrimSpace(r.PostFormValue("query_text"))
	scope := strings.TrimSpace(r.PostFormValue("scope_filter"))
	if msg := validateSearchQuery(name, queryText, scope); msg != "" {
		h.renderSearchQueriesForm(w, r, msg, "")
		return
	}
	if err := h.q.UpdateCodeSearchSavedQuery(r.Context(), h.d.Pool, usersdb.UpdateCodeSearchSavedQueryParams{
		ID:          id,
		UserID:      user.ID,
		Name:        name,
		QueryText:   queryText,
		Kind:        usersdb.CodeSearchQueryKindPlain,
		ScopeFilter: scope,
	}); err != nil {
		if isUniqueViolation(err) {
			h.renderSearchQueriesForm(w, r, "You already have a saved query with that name.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "settings/search-queries: update", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSearchQueriesForm(w, r, "", "Saved query updated.")
}

func (h *Handlers) settingsSearchQueryDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSearchQueriesForm(w, r, "Invalid saved-query id.", "")
		return
	}
	if err := h.q.DeleteCodeSearchSavedQuery(r.Context(), h.d.Pool, usersdb.DeleteCodeSearchSavedQueryParams{
		ID: id, UserID: user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/search-queries: delete", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSearchQueriesForm(w, r, "", "Saved query deleted.")
}

func (h *Handlers) renderSearchQueriesForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListCodeSearchSavedQueriesForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/search-queries: list", "user_id", user.ID, "error", err)
	}
	allowed, _, _ := h.advancedCodeSearchAllowed(r.Context(), user.ID)
	h.renderPage(w, r, "settings/search_queries", map[string]any{
		"Title":          "Saved search queries",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "search-queries",
		"Rows":           rows,
		"Allowed":        allowed,
		"FeatureKey":     string(entitlements.FeatureAdvancedCodeSearch),
		"MaxName":        searchQueryMaxNameChars,
		"MaxQueryText":   searchQueryMaxQueryTextChars,
		"MaxScope":       searchQueryMaxScopeChars,
		"Error":          errMsg,
		"Success":        successMsg,
	})
}

// advancedCodeSearchAllowed wraps the entitlement check and logs the
// would-deny for telemetry. Mirrors userSavedRepliesUnlimitedAllowed.
func (h *Handlers) advancedCodeSearchAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureAdvancedCodeSearch)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureAdvancedCodeSearch),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed, decision, nil
}

func validateSearchQuery(name, queryText, scope string) string {
	if name == "" {
		return "Name is required."
	}
	if len([]rune(name)) > searchQueryMaxNameChars {
		return "Name is too long (max 80 characters)."
	}
	if queryText == "" {
		return "Query text is required."
	}
	if len(queryText) > searchQueryMaxQueryTextChars {
		return "Query is too long (max 1000 characters)."
	}
	if len(scope) > searchQueryMaxScopeChars {
		return "Scope filter is too long (max 200 characters)."
	}
	return ""
}
