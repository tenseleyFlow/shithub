// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

// MountWebhooks registers the per-repo webhook CRUD + delivery views.
// Caller wraps with RequireUser; per-route policy gates inside.
//
// When the SecretBox isn't configured (operator hasn't set the
// AEAD key) the routes still register, but every handler short-
// circuits to a placeholder explaining the misconfiguration —
// staying consistent with the S32-shipped placeholder shape.
func (h *Handlers) MountWebhooks(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/webhooks", h.webhooksList)
	r.Get("/{owner}/{repo}/settings/webhooks/new", h.webhookNewForm)
	r.Post("/{owner}/{repo}/settings/webhooks", h.webhookCreate)
	r.Get("/{owner}/{repo}/settings/webhooks/{id}", h.webhookEditForm)
	r.Post("/{owner}/{repo}/settings/webhooks/{id}", h.webhookUpdate)
	r.Post("/{owner}/{repo}/settings/webhooks/{id}/delete", h.webhookDelete)
	r.Post("/{owner}/{repo}/settings/webhooks/{id}/toggle", h.webhookToggle)
	r.Post("/{owner}/{repo}/settings/webhooks/{id}/ping", h.webhookPing)
	r.Get("/{owner}/{repo}/settings/webhooks/{id}/deliveries/{deliveryID}", h.webhookDeliveryView)
	r.Post("/{owner}/{repo}/settings/webhooks/{id}/deliveries/{deliveryID}/redeliver", h.webhookRedeliver)
}

// webhooksList renders the per-repo webhook list. Replaces the S32
// placeholder.
func (h *Handlers) webhooksList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		h.renderWebhookPlaceholder(w, r, row, owner.Username, "Webhook delivery requires the at-rest secret key. Set Auth.TOTPKeyB64 in config and restart.")
		return
	}
	hooks, err := webhookdb.New().ListWebhooksForOwner(r.Context(), h.d.Pool, webhookdb.ListWebhooksForOwnerParams{
		OwnerKind: webhookdb.WebhookOwnerKindRepo,
		OwnerID:   row.ID,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "webhooks: list", "error", err)
		hooks = nil
	}
	notice := r.URL.Query().Get("notice")
	h.d.Render.RenderPage(w, r, "repo/settings_webhooks", map[string]any{
		"Title":          "Webhooks · " + row.Name,
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Webhooks":       hooks,
		"SettingsActive": "webhooks",
		"Notice":         settingsNoticeMessage(notice),
	})
}

// webhookNewForm renders the create form.
func (h *Handlers) webhookNewForm(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		h.renderWebhookPlaceholder(w, r, row, owner.Username, "Webhook delivery requires the at-rest secret key.")
		return
	}
	h.renderWebhookForm(w, r, row, owner.Username, nil, "", "")
}

// webhookCreate persists a new webhook + emits a ping.
func (h *Handlers) webhookCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		http.Error(w, "webhook key not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	params := webhook.CreateParams{
		OwnerKind:   "repo",
		OwnerID:     row.ID,
		URL:         strings.TrimSpace(r.PostFormValue("url")),
		ContentType: pickContentType(r.PostFormValue("content_type")),
		Events:      splitCommaList(r.PostFormValue("events")),
		Secret:      strings.TrimSpace(r.PostFormValue("secret")),
		Active:      r.PostFormValue("active") == "on",
		SSL:         r.PostFormValue("ssl_verification") == "on" || r.PostFormValue("ssl_verification") == "",
		ActorUserID: viewer.ID,
	}
	created, err := webhook.Create(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: webhook.DefaultSSRFConfig(),
	}, params)
	if err != nil {
		h.renderWebhookForm(w, r, row, owner.Username, &createFormState{
			URL: params.URL, ContentType: string(params.OwnerKind), Events: params.Events,
			Active: params.Active, SSL: params.SSL,
		}, friendlyWebhookError(err), "")
		return
	}
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		map[string]any{"action": "webhook_created", "webhook_id": created.ID, "url": params.URL})

	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks?notice=saved", http.StatusSeeOther)
}

// webhookEditForm renders the edit form for one webhook.
func (h *Handlers) webhookEditForm(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	deliveries, _ := webhookdb.New().ListDeliveriesForWebhook(r.Context(), h.d.Pool, webhookdb.ListDeliveriesForWebhookParams{
		WebhookID: hook.ID, Limit: 50,
	})
	h.d.Render.RenderPage(w, r, "repo/settings_webhook_edit", map[string]any{
		"Title":          hook.Url + " · webhook",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Webhook":        hook,
		"EventsCSV":      strings.Join(hook.Events, ", "),
		"Deliveries":     deliveries,
		"SettingsActive": "webhooks",
	})
}

// webhookUpdate persists an edit.
func (h *Handlers) webhookUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	params := webhook.UpdateParams{
		URL:         strings.TrimSpace(r.PostFormValue("url")),
		ContentType: pickContentType(r.PostFormValue("content_type")),
		Events:      splitCommaList(r.PostFormValue("events")),
		Active:      r.PostFormValue("active") == "on",
		SSL:         r.PostFormValue("ssl_verification") == "on" || r.PostFormValue("ssl_verification") == "",
		NewSecret:   strings.TrimSpace(r.PostFormValue("new_secret")),
	}
	if err := webhook.Update(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: webhook.DefaultSSRFConfig(),
	}, hook.ID, params); err != nil {
		http.Error(w, friendlyWebhookError(err), http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		map[string]any{"action": "webhook_updated", "webhook_id": hook.ID})
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks/"+strconv.FormatInt(hook.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// webhookDelete drops a webhook + cascades its deliveries.
func (h *Handlers) webhookDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	if err := webhook.Delete(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox,
	}, hook.ID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID,
		map[string]any{"action": "webhook_deleted", "webhook_id": hook.ID})
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks?notice=saved", http.StatusSeeOther)
}

// webhookToggle flips active true⇄false. Re-enabling a previously
// auto-disabled webhook resets the failure counter via SetActive.
func (h *Handlers) webhookToggle(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	if err := webhook.SetActive(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox,
	}, hook.ID, !hook.Active); err != nil {
		http.Error(w, "toggle failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks/"+strconv.FormatInt(hook.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// webhookPing enqueues a synthetic ping delivery.
func (h *Handlers) webhookPing(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	if err := webhook.EnqueuePing(r.Context(), webhook.FanoutDeps{
		Pool: h.d.Pool, Logger: h.d.Logger,
	}, hook.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "webhook ping", "error", err)
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks/"+strconv.FormatInt(hook.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// webhookDeliveryView shows one delivery's request/response.
func (h *Handlers) webhookDeliveryView(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	deliveryID, err := strconv.ParseInt(chi.URLParam(r, "deliveryID"), 10, 64)
	if err != nil || deliveryID <= 0 {
		http.Error(w, "bad delivery id", http.StatusBadRequest)
		return
	}
	delivery, err := webhookdb.New().GetDeliveryByID(r.Context(), h.d.Pool, deliveryID)
	if err != nil || delivery.WebhookID != hook.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	h.d.Render.RenderPage(w, r, "repo/settings_webhook_delivery", map[string]any{
		"Title":          "Delivery #" + strconv.FormatInt(delivery.ID, 10),
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner.Username,
		"Repo":           row,
		"Webhook":        hook,
		"Delivery":       delivery,
		"PayloadPretty":  prettyJSON(delivery.Payload),
		"ResponseBody":   string(delivery.ResponseBody),
		"SettingsActive": "webhooks",
	})
}

// webhookRedeliver clones a past delivery and enqueues it.
func (h *Handlers) webhookRedeliver(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedWebhook(w, r, row.ID)
	if !ok {
		return
	}
	originalID, err := strconv.ParseInt(chi.URLParam(r, "deliveryID"), 10, 64)
	if err != nil || originalID <= 0 {
		http.Error(w, "bad delivery id", http.StatusBadRequest)
		return
	}
	// Defense in depth: confirm the delivery belongs to this webhook.
	orig, err := webhookdb.New().GetDeliveryByID(r.Context(), h.d.Pool, originalID)
	if err != nil || orig.WebhookID != hook.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	newID, err := webhook.Redeliver(r.Context(), webhook.FanoutDeps{
		Pool: h.d.Pool, Logger: h.d.Logger,
	}, originalID)
	if err != nil {
		http.Error(w, "redeliver failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings/webhooks/"+strconv.FormatInt(hook.ID, 10)+"/deliveries/"+strconv.FormatInt(newID, 10), http.StatusSeeOther)
}

// loadOwnedWebhook resolves the URL `id` param and confirms it
// belongs to this repo. Writes 404 + returns false on miss.
func (h *Handlers) loadOwnedWebhook(w http.ResponseWriter, r *http.Request, repoID int64) (webhookdb.Webhook, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return webhookdb.Webhook{}, false
	}
	hook, err := webhookdb.New().GetWebhookByID(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return webhookdb.Webhook{}, false
	}
	if hook.OwnerKind != webhookdb.WebhookOwnerKindRepo || hook.OwnerID != repoID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return webhookdb.Webhook{}, false
	}
	return hook, true
}

// renderWebhookForm renders the create form (state may carry repopulated
// fields after a validation failure).
type createFormState struct {
	URL         string
	ContentType string
	Events      []string
	Active      bool
	SSL         bool
}

func (h *Handlers) renderWebhookForm(w http.ResponseWriter, r *http.Request, repo any, owner string, state *createFormState, errMsg string, notice string) {
	if state == nil {
		state = &createFormState{Active: true, SSL: true, ContentType: "json"}
	}
	h.d.Render.RenderPage(w, r, "repo/settings_webhook_new", map[string]any{
		"Title":          "New webhook",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner,
		"Repo":           repo,
		"Form":           state,
		"EventsCSV":      strings.Join(state.Events, ", "),
		"Error":          errMsg,
		"Notice":         notice,
		"SettingsActive": "webhooks",
	})
}

// renderWebhookPlaceholder reuses the deferred-tab template from S32
// when the operator hasn't configured the AEAD key.
func (h *Handlers) renderWebhookPlaceholder(w http.ResponseWriter, r *http.Request, repo any, owner, body string) {
	h.d.Render.RenderPage(w, r, "repo/settings_placeholder", map[string]any{
		"Title":          "Webhooks",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"Owner":          owner,
		"Repo":           repo,
		"Heading":        "Webhooks",
		"Body":           body,
		"SettingsActive": "webhooks",
	})
}

// pickContentType narrows the form input to the enum's two options.
func pickContentType(s string) string {
	switch strings.TrimSpace(s) {
	case "form":
		return "form"
	default:
		return "json"
	}
}

// friendlyWebhookError maps webhook orchestrator errors to user-facing
// strings. Falls back to the raw error so the operator gets something
// actionable in the form.
func friendlyWebhookError(err error) string {
	switch {
	case errors.Is(err, webhook.ErrBadURL):
		return "URL must be http or https with a host."
	case errors.Is(err, webhook.ErrBadContentType):
		return "Content type must be json or form."
	case errors.Is(err, webhook.ErrBadEvent):
		return "Event names must be 1–64 lowercase characters."
	}
	return err.Error()
}

// prettyJSON re-indents a JSON document so the delivery view shows it
// readably. Fall through to the raw bytes on parse failure.
func prettyJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}
