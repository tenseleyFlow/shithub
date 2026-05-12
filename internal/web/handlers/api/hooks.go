// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

// mountHooks registers the S50 §8 webhook REST surface for repos.
//
//	GET    /api/v1/repos/{o}/{r}/hooks                                 list
//	POST   /api/v1/repos/{o}/{r}/hooks                                 create
//	GET    /api/v1/repos/{o}/{r}/hooks/{id}                            fetch
//	PATCH  /api/v1/repos/{o}/{r}/hooks/{id}                            update
//	DELETE /api/v1/repos/{o}/{r}/hooks/{id}                            delete
//	GET    /api/v1/repos/{o}/{r}/hooks/{id}/deliveries                 list deliveries
//	GET    /api/v1/repos/{o}/{r}/hooks/{id}/deliveries/{did}           fetch delivery
//	POST   /api/v1/repos/{o}/{r}/hooks/{id}/deliveries/{did}/redeliver replay
//
// Policy: webhook management is admin-tier on the repo
// (`ActionRepoSettingsGeneral` is the minimum-role gate the HTML
// surface uses today). Secrets are never returned; they're set on
// create / rotated via the `secret` field on PATCH.
func (h *Handlers) mountHooks(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Get("/api/v1/repos/{owner}/{repo}/hooks", h.hooksList)
		r.Post("/api/v1/repos/{owner}/{repo}/hooks", h.hookCreate)
		r.Get("/api/v1/repos/{owner}/{repo}/hooks/{id}", h.hookGet)
		r.Patch("/api/v1/repos/{owner}/{repo}/hooks/{id}", h.hookPatch)
		r.Delete("/api/v1/repos/{owner}/{repo}/hooks/{id}", h.hookDelete)
		r.Get("/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries", h.hookDeliveriesList)
		r.Get("/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries/{did}", h.hookDeliveryGet)
		r.Post("/api/v1/repos/{owner}/{repo}/hooks/{id}/deliveries/{did}/redeliver", h.hookDeliveryRedeliver)
	})
}

// ─── presentation ───────────────────────────────────────────────────

type hookResponse struct {
	ID                 int64    `json:"id"`
	OwnerKind          string   `json:"owner_kind"`
	OwnerID            int64    `json:"owner_id"`
	URL                string   `json:"url"`
	ContentType        string   `json:"content_type"`
	Events             []string `json:"events"`
	Active             bool     `json:"active"`
	SSLVerification    bool     `json:"ssl_verification"`
	ConsecutiveFailure int32    `json:"consecutive_failures"`
	DisabledAt         string   `json:"disabled_at,omitempty"`
	DisabledReason     string   `json:"disabled_reason,omitempty"`
	LastSuccessAt      string   `json:"last_success_at,omitempty"`
	LastFailureAt      string   `json:"last_failure_at,omitempty"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
}

func presentHook(h webhookdb.Webhook) hookResponse {
	out := hookResponse{
		ID:                 h.ID,
		OwnerKind:          string(h.OwnerKind),
		OwnerID:            h.OwnerID,
		URL:                h.Url,
		ContentType:        string(h.ContentType),
		Events:             append([]string{}, h.Events...),
		Active:             h.Active,
		SSLVerification:    h.SslVerification,
		ConsecutiveFailure: h.ConsecutiveFailures,
		CreatedAt:          h.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:          h.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if h.DisabledAt.Valid {
		out.DisabledAt = h.DisabledAt.Time.UTC().Format(time.RFC3339)
	}
	if h.DisabledReason.Valid {
		out.DisabledReason = h.DisabledReason.String
	}
	if h.LastSuccessAt.Valid {
		out.LastSuccessAt = h.LastSuccessAt.Time.UTC().Format(time.RFC3339)
	}
	if h.LastFailureAt.Valid {
		out.LastFailureAt = h.LastFailureAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

type deliveryResponse struct {
	ID             int64  `json:"id"`
	HookID         int64  `json:"hook_id"`
	EventKind      string `json:"event_kind"`
	Status         string `json:"status"`
	ResponseStatus int32  `json:"response_status,omitempty"`
	Attempt        int32  `json:"attempt"`
	MaxAttempts    int32  `json:"max_attempts"`
	StartedAt      string `json:"started_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
	NextRetryAt    string `json:"next_retry_at,omitempty"`
	RedeliverOf    int64  `json:"redeliver_of,omitempty"`
	ErrorSummary   string `json:"error_summary,omitempty"`
	DeliveryUUID   string `json:"delivery_uuid,omitempty"`
}

func presentDelivery(d webhookdb.ListDeliveriesForWebhookPagedRow) deliveryResponse {
	out := deliveryResponse{
		ID:          d.ID,
		HookID:      d.WebhookID,
		EventKind:   d.EventKind,
		Status:      string(d.Status),
		Attempt:     d.Attempt,
		MaxAttempts: d.MaxAttempts,
	}
	if d.ResponseStatus.Valid {
		out.ResponseStatus = d.ResponseStatus.Int32
	}
	if d.StartedAt.Valid {
		out.StartedAt = d.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	if d.CompletedAt.Valid {
		out.CompletedAt = d.CompletedAt.Time.UTC().Format(time.RFC3339)
	}
	if d.NextRetryAt.Valid {
		out.NextRetryAt = d.NextRetryAt.Time.UTC().Format(time.RFC3339)
	}
	if d.RedeliverOf.Valid {
		out.RedeliverOf = d.RedeliverOf.Int64
	}
	if d.ErrorSummary.Valid {
		out.ErrorSummary = d.ErrorSummary.String
	}
	if d.DeliveryUuid.Valid {
		out.DeliveryUUID = formatUUID(d.DeliveryUuid.Bytes)
	}
	return out
}

func formatUUID(b [16]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for i, v := range b {
		out[pos] = hex[v>>4]
		out[pos+1] = hex[v&0x0f]
		pos += 2
		if i == 3 || i == 5 || i == 7 || i == 9 {
			out[pos] = '-'
			pos++
		}
	}
	return string(out)
}

// ─── list / get ─────────────────────────────────────────────────────

func (h *Handlers) hooksList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	rows, err := webhookdb.New().ListWebhooksForOwner(r.Context(), h.d.Pool, webhookdb.ListWebhooksForOwnerParams{
		OwnerKind: webhookdb.WebhookOwnerKindRepo,
		OwnerID:   repo.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list hooks", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]hookResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentHook(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) hookGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	hook, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presentHook(hook))
}

// ─── create / patch / delete ───────────────────────────────────────

type hookCreateRequest struct {
	URL             string   `json:"url"`
	ContentType     string   `json:"content_type"`
	Events          []string `json:"events"`
	Active          *bool    `json:"active"`
	SSLVerification *bool    `json:"ssl_verification"`
	Secret          string   `json:"secret"`
}

func (h *Handlers) hookCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	var body hookCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if h.d.SecretBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "webhook secrets not configured")
		return
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}
	ssl := true
	if body.SSLVerification != nil {
		ssl = *body.SSLVerification
	}
	if body.ContentType == "" {
		body.ContentType = "json"
	}
	row, err := webhook.Create(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: h.webhookSSRFConfig(),
	}, webhook.CreateParams{
		OwnerKind:   "repo",
		OwnerID:     repo.ID,
		URL:         body.URL,
		ContentType: body.ContentType,
		Events:      body.Events,
		Secret:      body.Secret,
		Active:      active,
		SSL:         ssl,
		ActorUserID: auth.UserID,
	})
	if err != nil {
		writeHookError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentHook(row))
}

type hookPatchRequest struct {
	URL             *string  `json:"url"`
	ContentType     *string  `json:"content_type"`
	Events          []string `json:"events"`
	Active          *bool    `json:"active"`
	SSLVerification *bool    `json:"ssl_verification"`
	Secret          string   `json:"secret"`
}

func (h *Handlers) hookPatch(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	cur, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "webhook secrets not configured")
		return
	}
	var body hookPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	url := cur.Url
	if body.URL != nil {
		url = *body.URL
	}
	ct := string(cur.ContentType)
	if body.ContentType != nil {
		ct = *body.ContentType
	}
	events := cur.Events
	if body.Events != nil {
		events = body.Events
	}
	active := cur.Active
	if body.Active != nil {
		active = *body.Active
	}
	ssl := cur.SslVerification
	if body.SSLVerification != nil {
		ssl = *body.SSLVerification
	}
	if err := webhook.Update(r.Context(), webhook.ManageDeps{
		Pool: h.d.Pool, SecretBox: h.d.SecretBox, SSRF: h.webhookSSRFConfig(),
	}, cur.ID, webhook.UpdateParams{
		URL: url, ContentType: ct, Events: events,
		Active: active, SSL: ssl, NewSecret: body.Secret,
	}); err != nil {
		writeHookError(w, err)
		return
	}
	fresh, _ := webhookdb.New().GetWebhookByID(r.Context(), h.d.Pool, cur.ID)
	writeJSON(w, http.StatusOK, presentHook(fresh))
}

func (h *Handlers) hookDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	cur, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if err := webhook.Delete(r.Context(), webhook.ManageDeps{Pool: h.d.Pool}, cur.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete hook", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── deliveries ─────────────────────────────────────────────────────

func (h *Handlers) hookDeliveriesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	hook, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := webhookdb.New()
	total, err := q.CountDeliveriesForWebhook(r.Context(), h.d.Pool, hook.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count deliveries", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListDeliveriesForWebhookPaged(r.Context(), h.d.Pool, webhookdb.ListDeliveriesForWebhookPagedParams{
		WebhookID: hook.ID,
		Limit:     int32(perPage),
		Offset:    int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list deliveries", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]deliveryResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentDelivery(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) hookDeliveryGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	hook, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	did, err := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "delivery not found")
		return
	}
	d, err := webhookdb.New().GetDeliveryByID(r.Context(), h.d.Pool, did)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "delivery not found")
		return
	}
	if d.WebhookID != hook.ID {
		writeAPIError(w, http.StatusNotFound, "delivery not found")
		return
	}
	// Present a richer per-delivery shape including the captured
	// response body — operators debugging a failed delivery want
	// the full transcript, which is why we don't surface it in the
	// list view (those rows are intentionally lighter).
	type detail struct {
		deliveryResponse
		RequestHeaders json.RawMessage `json:"request_headers,omitempty"`
		RequestBody    string          `json:"request_body,omitempty"`
		ResponseBody   string          `json:"response_body,omitempty"`
	}
	base := presentDelivery(webhookdb.ListDeliveriesForWebhookPagedRow{
		ID: d.ID, WebhookID: d.WebhookID, EventKind: d.EventKind,
		EventID: d.EventID, DeliveryUuid: d.DeliveryUuid,
		ResponseStatus: d.ResponseStatus, ResponseTruncated: d.ResponseTruncated,
		StartedAt: d.StartedAt, CompletedAt: d.CompletedAt,
		Attempt: d.Attempt, MaxAttempts: d.MaxAttempts,
		NextRetryAt: d.NextRetryAt, Status: d.Status,
		ErrorSummary: d.ErrorSummary, RedeliverOf: d.RedeliverOf,
	})
	out := detail{deliveryResponse: base}
	if len(d.RequestHeaders) > 0 {
		out.RequestHeaders = d.RequestHeaders
	}
	if len(d.RequestBody) > 0 {
		out.RequestBody = string(d.RequestBody)
	}
	if len(d.ResponseBody) > 0 {
		out.ResponseBody = string(d.ResponseBody)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) hookDeliveryRedeliver(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	hook, ok := h.resolveHook(w, r, repo.ID, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	did, err := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "delivery not found")
		return
	}
	cur, err := webhookdb.New().GetDeliveryByID(r.Context(), h.d.Pool, did)
	if err != nil || cur.WebhookID != hook.ID {
		writeAPIError(w, http.StatusNotFound, "delivery not found")
		return
	}
	newID, err := webhook.Redeliver(r.Context(), webhook.FanoutDeps{Pool: h.d.Pool}, did)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: redeliver", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "redeliver failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": newID})
}

// ─── helpers ────────────────────────────────────────────────────────

func (h *Handlers) resolveHook(w http.ResponseWriter, r *http.Request, repoID int64, idRaw string) (webhookdb.Webhook, bool) {
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "hook not found")
		return webhookdb.Webhook{}, false
	}
	hook, err := webhookdb.New().GetWebhookByID(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "hook not found")
			return webhookdb.Webhook{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup hook", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return webhookdb.Webhook{}, false
	}
	if hook.OwnerKind != webhookdb.WebhookOwnerKindRepo || hook.OwnerID != repoID {
		writeAPIError(w, http.StatusNotFound, "hook not found")
		return webhookdb.Webhook{}, false
	}
	return hook, true
}

// webhookSSRFConfig returns the per-instance SSRF policy. Default is
// the production-strict config; tests override h.d.WebhookSSRF to
// permit loopback URLs.
func (h *Handlers) webhookSSRFConfig() webhook.SSRFConfig {
	if len(h.d.WebhookSSRF.AllowedSchemes) == 0 && !h.d.WebhookSSRF.AllowPrivateNetworks {
		return webhook.DefaultSSRFConfig()
	}
	return h.d.WebhookSSRF
}

func writeHookError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, webhook.ErrBadURL),
		errors.Is(err, webhook.ErrBadContentType),
		errors.Is(err, webhook.ErrBadOwnerKind),
		errors.Is(err, webhook.ErrBadEvent):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
