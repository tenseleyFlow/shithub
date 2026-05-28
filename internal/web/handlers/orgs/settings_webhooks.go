// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

func (h *Handlers) settingsHooks(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		h.renderOrgWebhooksList(w, r, org, nil, "Webhook delivery requires the at-rest secret key.", "")
		return
	}
	hooks, err := webhookdb.New().ListWebhooksForOwner(r.Context(), h.d.Pool, webhookdb.ListWebhooksForOwnerParams{
		OwnerKind: webhookdb.WebhookOwnerKindOrg,
		OwnerID:   org.ID,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org webhooks: list", "org_id", org.ID, "error", err)
		hooks = nil
	}
	h.renderOrgWebhooksList(w, r, org, hooks, "", orgWebhookNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsHookNew(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		h.renderOrgWebhooksList(w, r, org, nil, "Webhook delivery requires the at-rest secret key.", "")
		return
	}
	h.renderOrgWebhookForm(w, r, org, nil, "")
}

func (h *Handlers) settingsHookCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
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
		OwnerKind:   "org",
		OwnerID:     org.ID,
		URL:         strings.TrimSpace(r.PostFormValue("url")),
		ContentType: orgWebhookContentType(r.PostFormValue("content_type")),
		Events:      orgWebhookEvents(r.PostFormValue("events")),
		Secret:      strings.TrimSpace(r.PostFormValue("secret")),
		Active:      r.PostFormValue("active") == "on",
		SSL:         r.PostFormValue("ssl_verification") == "on" || r.PostFormValue("ssl_verification") == "",
		ActorUserID: viewer.ID,
	}
	created, err := webhook.Create(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: h.webhookSSRFConfig(),
	}, params)
	if err != nil {
		h.renderOrgWebhookForm(w, r, org, &orgWebhookFormState{
			URL: params.URL, ContentType: params.ContentType, Events: params.Events,
			Active: params.Active, SSL: params.SSL,
		}, friendlyOrgWebhookError(err))
		return
	}
	h.recordOrgWebhookAudit(r, viewer, audit.ActionWebhookCreated, org.ID, map[string]any{
		"webhook_id": created.ID,
		"url":        params.URL,
	})
	http.Redirect(w, r, orgHooksPath(org.Slug)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsHookEdit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	deliveries, _ := webhookdb.New().ListDeliveriesForWebhook(r.Context(), h.d.Pool, webhookdb.ListDeliveriesForWebhookParams{
		WebhookID: hook.ID,
		Limit:     50,
	})
	h.d.Render.RenderPage(w, r, "orgs/settings_hook_edit", map[string]any{
		"Title":             org.Slug + " · webhook",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"Webhook":           hook,
		"EventsCSV":         strings.Join(hook.Events, ", "),
		"Deliveries":        deliveries,
		"OrgSettingsActive": "integrations",
	})
}

func (h *Handlers) settingsHookUpdate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	params := webhook.UpdateParams{
		URL:         strings.TrimSpace(r.PostFormValue("url")),
		ContentType: orgWebhookContentType(r.PostFormValue("content_type")),
		Events:      orgWebhookEvents(r.PostFormValue("events")),
		Active:      r.PostFormValue("active") == "on",
		SSL:         r.PostFormValue("ssl_verification") == "on" || r.PostFormValue("ssl_verification") == "",
		NewSecret:   strings.TrimSpace(r.PostFormValue("new_secret")),
	}
	if err := webhook.Update(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: h.webhookSSRFConfig(),
	}, hook.ID, params); err != nil {
		http.Error(w, friendlyOrgWebhookError(err), http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgWebhookAudit(r, viewer, audit.ActionWebhookUpdated, org.ID, map[string]any{"webhook_id": hook.ID})
	http.Redirect(w, r, orgHookPath(org.Slug, hook.ID)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsHookDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	if err := webhook.Delete(r.Context(), webhook.ManageDeps{Pool: h.d.Pool, SecretBox: h.d.SecretBox}, hook.ID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgWebhookAudit(r, viewer, audit.ActionWebhookDeleted, org.ID, map[string]any{
		"webhook_id": hook.ID,
		"url":        hook.Url,
	})
	http.Redirect(w, r, orgHooksPath(org.Slug)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsHookToggle(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	newActive := !hook.Active
	if err := webhook.SetActive(r.Context(), webhook.ManageDeps{Pool: h.d.Pool, SecretBox: h.d.SecretBox}, hook.ID, newActive); err != nil {
		http.Error(w, "toggle failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	action := audit.ActionWebhookActiveSet
	if !newActive {
		action = audit.ActionWebhookActiveUnset
	}
	h.recordOrgWebhookAudit(r, viewer, action, org.ID, map[string]any{"webhook_id": hook.ID})
	http.Redirect(w, r, orgHookPath(org.Slug, hook.ID)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsHookPing(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	if err := webhook.EnqueuePing(r.Context(), webhook.FanoutDeps{
		Pool: h.d.Pool, Logger: h.d.Logger,
	}, hook.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "org webhook ping", "webhook_id", hook.ID, "error", err)
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgWebhookAudit(r, viewer, audit.ActionWebhookPinged, org.ID, map[string]any{"webhook_id": hook.ID})
	http.Redirect(w, r, orgHookPath(org.Slug, hook.ID)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsHookDelivery(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
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
	h.d.Render.RenderPage(w, r, "orgs/settings_hook_delivery", map[string]any{
		"Title":             "Delivery #" + strconv.FormatInt(delivery.ID, 10),
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"Webhook":           hook,
		"Delivery":          delivery,
		"PayloadPretty":     prettyOrgWebhookJSON(delivery.Payload),
		"ResponseBody":      string(delivery.ResponseBody),
		"OrgSettingsActive": "integrations",
	})
}

func (h *Handlers) settingsHookRedeliver(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	hook, ok := h.loadOwnedOrgWebhook(w, r, org.ID)
	if !ok {
		return
	}
	originalID, err := strconv.ParseInt(chi.URLParam(r, "deliveryID"), 10, 64)
	if err != nil || originalID <= 0 {
		http.Error(w, "bad delivery id", http.StatusBadRequest)
		return
	}
	orig, err := webhookdb.New().GetDeliveryByID(r.Context(), h.d.Pool, originalID)
	if err != nil || orig.WebhookID != hook.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	newID, err := webhook.Redeliver(r.Context(), webhook.FanoutDeps{Pool: h.d.Pool, Logger: h.d.Logger}, originalID)
	if err != nil {
		http.Error(w, "redeliver failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgWebhookAudit(r, viewer, audit.ActionWebhookRedelivered, org.ID, map[string]any{
		"webhook_id":           hook.ID,
		"original_delivery_id": originalID,
		"new_delivery_id":      newID,
	})
	http.Redirect(w, r, orgHookDeliveryPath(org.Slug, hook.ID, newID), http.StatusSeeOther)
}

func (h *Handlers) renderOrgWebhooksList(w http.ResponseWriter, r *http.Request, org orgsdb.Org, hooks []webhookdb.Webhook, setupErr, notice string) {
	h.d.Render.RenderPage(w, r, "orgs/settings_hooks", map[string]any{
		"Title":             org.Slug + " · Webhooks",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"Webhooks":          hooks,
		"SetupError":        setupErr,
		"Notice":            notice,
		"OrgSettingsActive": "integrations",
	})
}

type orgWebhookFormState struct {
	URL         string
	ContentType string
	Events      []string
	Active      bool
	SSL         bool
}

func (h *Handlers) renderOrgWebhookForm(w http.ResponseWriter, r *http.Request, org orgsdb.Org, state *orgWebhookFormState, errMsg string) {
	if state == nil {
		state = &orgWebhookFormState{Active: true, SSL: true, ContentType: "json"}
	}
	h.d.Render.RenderPage(w, r, "orgs/settings_hook_new", map[string]any{
		"Title":             org.Slug + " · New webhook",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"Form":              state,
		"EventsCSV":         strings.Join(state.Events, ", "),
		"Error":             errMsg,
		"OrgSettingsActive": "integrations",
	})
}

func (h *Handlers) loadOwnedOrgWebhook(w http.ResponseWriter, r *http.Request, orgID int64) (webhookdb.Webhook, bool) {
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
	if hook.OwnerKind != webhookdb.WebhookOwnerKindOrg || hook.OwnerID != orgID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return webhookdb.Webhook{}, false
	}
	return hook, true
}

func (h *Handlers) webhookSSRFConfig() webhook.SSRFConfig {
	return h.d.WebhookSSRF
}

func (h *Handlers) recordOrgWebhookAudit(r *http.Request, viewer middleware.CurrentUser, action audit.Action, orgID int64, meta map[string]any) {
	auditActor, auditMeta := viewer.AuditActor(meta)
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, auditActor, action, audit.TargetOrg, orgID, auditMeta)
}

func orgWebhookContentType(s string) string {
	switch strings.TrimSpace(s) {
	case "form":
		return "form"
	default:
		return "json"
	}
}

func orgWebhookEvents(s string) []string {
	return splitOrgWebhookList(s)
}

func splitOrgWebhookList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func friendlyOrgWebhookError(err error) string {
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

func orgWebhookNoticeMessage(code string) string {
	switch code {
	case "saved":
		return "Webhook settings saved."
	default:
		return ""
	}
}

func prettyOrgWebhookJSON(raw []byte) string {
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

func orgHooksPath(slug string) string {
	return "/organizations/" + slug + "/settings/hooks"
}

func orgHookPath(slug string, hookID int64) string {
	return orgHooksPath(slug) + "/" + strconv.FormatInt(hookID, 10)
}

func orgHookDeliveryPath(slug string, hookID, deliveryID int64) string {
	return orgHookPath(slug, hookID) + "/deliveries/" + strconv.FormatInt(deliveryID, 10)
}
