// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

// PRO-EXT01-13c: settings page for the automation bucket — webhook
// relays + cron-scheduled workflow_dispatch. Free users see the page
// in a Pro-locked state (forms disabled, banner + upgrade CTA); Pro
// users get full CRUD.
//
// HTML CRUD intentionally narrow: create + delete only. Disable
// (vs delete) and edit lives in the REST surface for now — the
// settings page is the discovery + at-a-glance view; advanced state
// changes happen via the REST API. Trims template surface area and
// matches how the Actions secrets page is shaped.

// settingsAutomationForm renders GET /settings/automation.
func (h *Handlers) settingsAutomationForm(w http.ResponseWriter, r *http.Request) {
	h.renderAutomationPage(w, r, "", "")
}

func (h *Handlers) renderAutomationPage(w http.ResponseWriter, r *http.Request, relayError, cronError string) {
	user := middleware.CurrentUserFromContext(r.Context())
	allowed, _, _ := h.userAutomationAllowed(r.Context(), user.ID)

	// Relays + cron lists are best-effort: a failure shows the page
	// with an empty list rather than 500-ing the whole settings UI.
	var relays []webhookrelay.Relay
	if h.d.SecretBox != nil {
		var err error
		relays, err = (webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}).
			ListForUser(r.Context(), user.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "automation: list relays", "error", err)
		}
	}
	cronRows, err := (cronworkflow.Deps{Pool: h.d.Pool}).ListForUser(r.Context(), user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "automation: list cron", "error", err)
	}

	// Reuse the template's pro-lock-cta the same way actions_secrets
	// does — point at the umbrella relay feature key. (Cron and relay
	// share the same "automation" lock surface for product copy.)
	h.renderPage(w, r, "settings/automation", map[string]any{
		"Title":          "Automation",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "automation",
		"Allowed":        allowed,
		"FineGrainedKey": string(entitlements.FeatureWebhookRelay),
		"Relays":         relays,
		"CronDispatches": cronRows,
		"RelayError":     relayError,
		"CronError":      cronError,
		"BaseURL":        h.d.BaseURL,
	})
}

// userAutomationAllowed checks FeatureWebhookRelay AND emits the
// report_only_deny log line. We gate the page on relays specifically
// — cron uses the same Pro tier so the practical answer is the same.
func (h *Handlers) userAutomationAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureWebhookRelay)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed && h.d.Logger != nil {
		mode := "report_only"
		if h.d.BillingEnforce.UserWebhookRelay {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureWebhookRelay),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "settings-automation")
	}
	return decision.Allowed, decision, nil
}

// settingsAutomationRelayCreate handles POST /settings/automation/relays.
// Form fields: name, destination_url_1..N (each non-empty URL becomes
// a destination). The raw token is shown one-shot in a banner on the
// next render — UI uses a flash message rather than redirect so the
// user sees it once.
func (h *Handlers) settingsAutomationRelayCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	allowed, decision, derr := h.userAutomationAllowed(r.Context(), user.ID)
	if derr != nil {
		h.d.Logger.ErrorContext(r.Context(), "automation: entitlement", "error", derr)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserWebhookRelay {
		banner := decision.PrincipalUpgradeBanner("webhook relays",
			billing.PrincipalForUser(user.ID), "")
		h.renderAutomationPage(w, r, banner.Message, "")
		return
	}
	if h.d.SecretBox == nil {
		h.renderAutomationPage(w, r,
			"Operator has not configured a secret box; cannot store relays.", "")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	dests := parseRelayDestinations(r)
	hmacSecret, err := newRelayHMACSecret()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "automation: hmac gen", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	_, err = (webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}).Create(
		r.Context(),
		webhookrelay.CreateInput{
			UserID: user.ID, Name: name, HMACSecret: hmacSecret,
			Destinations: dests,
		},
	)
	switch {
	case errors.Is(err, webhookrelay.ErrEmptyName):
		h.renderAutomationPage(w, r, "Name is required.", "")
		return
	case errors.Is(err, webhookrelay.ErrTooManyDestinations):
		h.renderAutomationPage(w, r, err.Error(), "")
		return
	case err != nil:
		h.d.Logger.ErrorContext(r.Context(), "automation: create relay", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/automation", http.StatusSeeOther)
}

func (h *Handlers) settingsAutomationRelayDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, ok := parseInt64URLParam(w, r, "id")
	if !ok {
		return
	}
	// Ownership check via GetByID.
	deps := webhookrelay.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}
	relay, _, err := deps.GetByID(r.Context(), id)
	if err != nil || relay.UserID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := deps.Delete(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "automation: delete relay", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/automation", http.StatusSeeOther)
}

func (h *Handlers) settingsAutomationCronCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	allowed, decision, derr := h.userAutomationAllowed(r.Context(), user.ID)
	if derr != nil {
		h.d.Logger.ErrorContext(r.Context(), "automation: entitlement", "error", derr)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserCronWorkflowDispatch {
		banner := decision.PrincipalUpgradeBanner("cron workflow dispatch",
			billing.PrincipalForUser(user.ID), "")
		h.renderAutomationPage(w, r, "", banner.Message)
		return
	}
	repoIDRaw := r.PostFormValue("repo_id")
	repoID, _ := strconv.ParseInt(repoIDRaw, 10, 64)
	workflowFile := strings.TrimSpace(r.PostFormValue("workflow_file"))
	ref := strings.TrimSpace(r.PostFormValue("ref"))
	if !strings.HasPrefix(ref, "refs/") && ref != "" {
		ref = "refs/heads/" + ref
	}
	cronExpr := strings.TrimSpace(r.PostFormValue("cron_expr"))

	// Cheap ownership check — must own the target repo.
	if !assertRepoOwnedByUser(r.Context(), h.d.Pool, repoID, user.ID) {
		h.renderAutomationPage(w, r, "", "Repository not found or not owned by you.")
		return
	}

	_, err := (cronworkflow.Deps{Pool: h.d.Pool}).Create(r.Context(),
		cronworkflow.CreateInput{
			UserID:       user.ID,
			RepoID:       repoID,
			WorkflowFile: workflowFile,
			Ref:          ref,
			CronExpr:     cronExpr,
		})
	switch {
	case errors.Is(err, cronworkflow.ErrEmptyWorkflow):
		h.renderAutomationPage(w, r, "", "Workflow file is required.")
		return
	case errors.Is(err, cronworkflow.ErrEmptyRef):
		h.renderAutomationPage(w, r, "", "Ref is required.")
		return
	case errors.Is(err, cronworkflow.ErrInvalidCronExpr):
		h.renderAutomationPage(w, r, "", err.Error())
		return
	case err != nil:
		h.d.Logger.ErrorContext(r.Context(), "automation: create cron", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/automation", http.StatusSeeOther)
}

func (h *Handlers) settingsAutomationCronDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, ok := parseInt64URLParam(w, r, "id")
	if !ok {
		return
	}
	d, err := (cronworkflow.Deps{Pool: h.d.Pool}).GetByID(r.Context(), id)
	if err != nil || d.UserID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := (cronworkflow.Deps{Pool: h.d.Pool}).Delete(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "automation: delete cron", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/automation", http.StatusSeeOther)
}

// parseRelayDestinations reads destination_url_1, _2, … out of the
// form and returns the non-empty ones. Cap matches webhookrelay.
// MaxDestinations.
func parseRelayDestinations(r *http.Request) []webhookrelay.Destination {
	out := make([]webhookrelay.Destination, 0, webhookrelay.MaxDestinations)
	for i := 1; i <= webhookrelay.MaxDestinations; i++ {
		v := strings.TrimSpace(r.PostFormValue("destination_url_" + strconv.Itoa(i)))
		if v == "" {
			continue
		}
		out = append(out, webhookrelay.Destination{URL: v})
	}
	return out
}

func newRelayHMACSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func parseInt64URLParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// assertRepoOwnedByUser is the auth-layer ownership check used by the
// cron-create form. Mirrors the api-layer check from cron_workflow_crud.go;
// inlined here to avoid pulling api into the auth package (cycle).
func assertRepoOwnedByUser(ctx context.Context, pool *pgxpool.Pool, repoID, userID int64) bool {
	var ownerUserID int64
	if err := pool.QueryRow(
		ctx,
		`SELECT coalesce(owner_user_id, 0) FROM repos WHERE id = $1`, repoID,
	).Scan(&ownerUserID); err != nil {
		return false
	}
	return ownerUserID == userID
}
