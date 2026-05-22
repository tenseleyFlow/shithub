// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/secretscan"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountSecretScanning registers the SP26c read-only Secret Protection API.
//
//	GET /api/v1/repos/{owner}/{repo}/secret-scanning/status
//	GET /api/v1/repos/{owner}/{repo}/secret-scanning/alerts
//	GET /api/v1/repos/{owner}/{repo}/secret-scanning/allowlist
//	GET /api/v1/repos/{owner}/{repo}/secret-scanning/bypass-requests
//
// Secret scanning metadata is sensitive even when the repository is public, so
// these routes use the repo settings policy action instead of plain repo read.
// The OAuth/PAT scope remains repo:read because the endpoints are read-only.
func (h *Handlers) mountSecretScanning(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/secret-scanning/status", h.secretScanningStatusGet)
		r.Get("/api/v1/repos/{owner}/{repo}/secret-scanning/alerts", h.secretScanningAlertsList)
		r.Get("/api/v1/repos/{owner}/{repo}/secret-scanning/allowlist", h.secretScanningAllowlistList)
		r.Get("/api/v1/repos/{owner}/{repo}/secret-scanning/bypass-requests", h.secretScanningBypassRequestsList)
	})
}

type secretScanningGate struct {
	Allowed    bool
	FeatureKey string
	Message    string
}

func (h *Handlers) secretScanningGate(ctx context.Context, repo reposdb.Repo) secretScanningGate {
	gate := secretScanningGate{Allowed: true}
	if !secretScanningRepoIsPrivate(repo) {
		return gate
	}
	if repo.OwnerOrgID.Valid {
		gate.FeatureKey = string(entitlements.FeatureSecretScanning)
		gate.Message = "secret scanning requires Team for private organization repositories"
		decision, err := entitlements.CheckPrincipalFeature(ctx,
			entitlements.Deps{Pool: h.d.Pool},
			billing.PrincipalForOrg(repo.OwnerOrgID.Int64),
			entitlements.FeatureSecretScanning)
		if err != nil {
			gate.Allowed = false
			return gate
		}
		gate.Allowed = decision.Allowed
		return gate
	}
	if repo.OwnerUserID.Valid && h.d.BillingEnforce.UserSecretScanHistory {
		gate.FeatureKey = string(entitlements.FeatureSecretScanHistory)
		gate.Message = "secret scan history requires Pro for private personal repositories"
		decision, err := entitlements.CheckPrincipalFeature(ctx,
			entitlements.Deps{Pool: h.d.Pool},
			billing.PrincipalForUser(repo.OwnerUserID.Int64),
			entitlements.FeatureSecretScanHistory)
		if err != nil {
			gate.Allowed = false
			return gate
		}
		gate.Allowed = decision.Allowed
	}
	return gate
}

func (h *Handlers) secretBypassGate(ctx context.Context, repo reposdb.Repo) secretScanningGate {
	gate := secretScanningGate{Allowed: true}
	if !secretScanningRepoIsPrivate(repo) || !repo.OwnerOrgID.Valid {
		return gate
	}
	gate.FeatureKey = string(entitlements.FeatureSecretBypassControls)
	gate.Message = "secret push-protection bypass controls require Team for private organization repositories"
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(repo.OwnerOrgID.Int64),
		entitlements.FeatureSecretBypassControls)
	if err != nil {
		gate.Allowed = false
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

func secretScanningRepoIsPrivate(repo reposdb.Repo) bool {
	return policy.RepoRef{Visibility: string(repo.Visibility)}.IsPrivate()
}

func writeSecretScanningGateError(w http.ResponseWriter, gate secretScanningGate) {
	msg := gate.Message
	if msg == "" {
		msg = "secret scanning is unavailable for this repository"
	}
	body := map[string]string{"error": msg}
	if gate.FeatureKey != "" {
		body["feature"] = gate.FeatureKey
	}
	writeJSON(w, http.StatusPaymentRequired, body)
}

func (h *Handlers) secretScanningStatusGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	gate := h.secretScanningGate(r.Context(), *repo)
	if !gate.Allowed {
		writeSecretScanningGateError(w, gate)
		return
	}

	sq := secretscandb.New()
	counts, err := h.secretScanningFindingCounts(r.Context(), *repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: secret scanning status counts", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning status failed")
		return
	}
	allowlist, err := sq.ListSecretScanAllowlistForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: secret scanning allowlist count", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning status failed")
		return
	}
	var bypassCount int
	bypassGate := h.secretBypassGate(r.Context(), *repo)
	if bypassGate.Allowed {
		rows, err := sq.ListSecretScanBypassRequestsForRepo(r.Context(), h.d.Pool, repo.ID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: secret scanning bypass count", "repo_id", repo.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "secret scanning status failed")
			return
		}
		bypassCount = len(rows)
	}

	writeJSON(w, http.StatusOK, secretScanningStatusResponse{
		Enabled:                        true,
		Visibility:                     string(repo.Visibility),
		FeatureKey:                     gate.FeatureKey,
		TotalAlertCount:                counts.Total,
		OpenAlertCount:                 counts.Open,
		ResolvedAlertCount:             counts.Resolved,
		AllowlistedAlertCount:          counts.Allowlisted,
		StaleAlertCount:                counts.Stale,
		AllowlistCount:                 len(allowlist),
		BypassControlsAvailable:        bypassGate.Allowed,
		BypassControlsFeatureKey:       bypassGate.FeatureKey,
		BypassRequestCount:             bypassCount,
		LatestFindingObservedAt:        counts.LatestObservedAt,
		ScanHistoryBacking:             "findings",
		RawSecretMaterialIncluded:      false,
		ValidityChecksAvailable:        false,
		ProviderNotificationsAvailable: false,
	})
}

func (h *Handlers) secretScanningAlertsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	gate := h.secretScanningGate(r.Context(), *repo)
	if !gate.Allowed {
		writeSecretScanningGateError(w, gate)
		return
	}

	status, err := secretScanningStatusFilter(r.URL.Query().Get("status"))
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	sq := secretscandb.New()
	total, err := sq.CountSecretScanFindingsForRepo(r.Context(), h.d.Pool, secretscandb.CountSecretScanFindingsForRepoParams{
		RepoID:       repo.ID,
		StatusFilter: status,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count secret scan alerts", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning alerts failed")
		return
	}
	rows, err := sq.ListSecretScanFindingsForRepo(r.Context(), h.d.Pool, secretscandb.ListSecretScanFindingsForRepoParams{
		RepoID:       repo.ID,
		StatusFilter: status,
		Limit:        int32(perPage),
		Offset:       int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list secret scan alerts", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning alerts failed")
		return
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]secretScanningAlertResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentSecretScanningAlert(row, chi.URLParam(r, "owner"), repo.Name, h.d.BaseURL))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) secretScanningAllowlistList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	gate := h.secretScanningGate(r.Context(), *repo)
	if !gate.Allowed {
		writeSecretScanningGateError(w, gate)
		return
	}
	rows, err := secretscandb.New().ListSecretScanAllowlistForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list secret scan allowlist", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning allowlist failed")
		return
	}
	actors := h.resolveUserEnvelopesBatch(r.Context(), allowlistActorIDs(rows))
	out := make([]secretScanningAllowlistResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentSecretScanningAllowlist(row, actors))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(out),
		"allowlist":   out,
	})
}

func (h *Handlers) secretScanningBypassRequestsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	scanGate := h.secretScanningGate(r.Context(), *repo)
	if !scanGate.Allowed {
		writeSecretScanningGateError(w, scanGate)
		return
	}
	gate := h.secretBypassGate(r.Context(), *repo)
	if !gate.Allowed {
		writeSecretScanningGateError(w, gate)
		return
	}
	rows, err := secretscandb.New().ListSecretScanBypassRequestsForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list secret scan bypass requests", "repo_id", repo.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "secret scanning bypass requests failed")
		return
	}
	actors := h.resolveUserEnvelopesBatch(r.Context(), bypassActorIDs(rows))
	out := make([]secretScanningBypassRequestResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentSecretScanningBypassRequest(row, actors))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":     len(out),
		"bypass_requests": out,
	})
}

type secretScanningCounts struct {
	Total            int64
	Open             int64
	Resolved         int64
	Allowlisted      int64
	Stale            int64
	LatestObservedAt string
}

func (h *Handlers) secretScanningFindingCounts(ctx context.Context, repo reposdb.Repo) (secretScanningCounts, error) {
	sq := secretscandb.New()
	count := func(status string) (int64, error) {
		return sq.CountSecretScanFindingsForRepo(ctx, h.d.Pool, secretscandb.CountSecretScanFindingsForRepoParams{
			RepoID:       repo.ID,
			StatusFilter: status,
		})
	}
	total, err := count("")
	if err != nil {
		return secretScanningCounts{}, err
	}
	open, err := count(string(secretscandb.SecretScanFindingStatusOpen))
	if err != nil {
		return secretScanningCounts{}, err
	}
	resolved, err := count(string(secretscandb.SecretScanFindingStatusResolved))
	if err != nil {
		return secretScanningCounts{}, err
	}
	allowlisted, err := count(string(secretscandb.SecretScanFindingStatusAllowlisted))
	if err != nil {
		return secretScanningCounts{}, err
	}
	stale, err := count(string(secretscandb.SecretScanFindingStatusStale))
	if err != nil {
		return secretScanningCounts{}, err
	}
	latest, err := sq.GetLatestSecretScanFindingObservedAt(ctx, h.d.Pool, repo.ID)
	if err != nil {
		return secretScanningCounts{}, err
	}
	latestObservedAt := ""
	if latest.Valid {
		latestObservedAt = pgTimestampString(latest)
	}
	return secretScanningCounts{
		Total:            total,
		Open:             open,
		Resolved:         resolved,
		Allowlisted:      allowlisted,
		Stale:            stale,
		LatestObservedAt: latestObservedAt,
	}, nil
}

type secretScanningStatusResponse struct {
	Enabled                        bool   `json:"enabled"`
	Visibility                     string `json:"visibility"`
	FeatureKey                     string `json:"feature_key,omitempty"`
	TotalAlertCount                int64  `json:"total_alert_count"`
	OpenAlertCount                 int64  `json:"open_alert_count"`
	ResolvedAlertCount             int64  `json:"resolved_alert_count"`
	AllowlistedAlertCount          int64  `json:"allowlisted_alert_count"`
	StaleAlertCount                int64  `json:"stale_alert_count"`
	AllowlistCount                 int    `json:"allowlist_count"`
	BypassControlsAvailable        bool   `json:"bypass_controls_available"`
	BypassControlsFeatureKey       string `json:"bypass_controls_feature_key,omitempty"`
	BypassRequestCount             int    `json:"bypass_request_count,omitempty"`
	LatestFindingObservedAt        string `json:"latest_finding_observed_at,omitempty"`
	ScanHistoryBacking             string `json:"scan_history_backing"`
	RawSecretMaterialIncluded      bool   `json:"raw_secret_material_included"`
	ValidityChecksAvailable        bool   `json:"validity_checks_available"`
	ProviderNotificationsAvailable bool   `json:"provider_notifications_available"`
}

type secretScanningAlertResponse struct {
	ID                             int64                         `json:"id"`
	Number                         int64                         `json:"number"`
	State                          string                        `json:"state"`
	Status                         string                        `json:"status"`
	SecretType                     string                        `json:"secret_type"`
	SecretTypeDisplayName          string                        `json:"secret_type_display_name"`
	ProviderSlug                   string                        `json:"provider_slug,omitempty"`
	PatternCategory                string                        `json:"pattern_category"`
	Validity                       string                        `json:"validity"`
	ValidityCheck                  secretScanningCapabilityState `json:"validity_check"`
	ProviderNotification           string                        `json:"provider_notification"`
	ProviderNotificationCapability secretScanningCapabilityState `json:"provider_notification_capability"`
	Path                           string                        `json:"path"`
	Line                           int32                         `json:"line"`
	CommitSHA                      string                        `json:"commit_sha"`
	FirstSeenSHA                   string                        `json:"first_seen_sha"`
	CreatedAt                      string                        `json:"created_at"`
	UpdatedAt                      string                        `json:"updated_at"`
	ResolvedAt                     string                        `json:"resolved_at,omitempty"`
	HTMLURL                        string                        `json:"html_url,omitempty"`
}

type secretScanningCapabilityState struct {
	SupportedByGitHub   bool   `json:"supported_by_github"`
	SupportedByInstance bool   `json:"supported_by_instance"`
	Status              string `json:"status"`
	Description         string `json:"description"`
}

func presentSecretScanningAlert(row secretscandb.SecretScanFinding, owner, repoName, baseURL string) secretScanningAlertResponse {
	capability := secretscan.CapabilityForPattern(row.Pattern)
	validity := capability.ValidityState()
	providerNotification := capability.ProviderNotificationState()
	out := secretScanningAlertResponse{
		ID:                    row.ID,
		Number:                row.ID,
		State:                 secretScanningAlertState(row.Status),
		Status:                string(row.Status),
		SecretType:            capability.SecretType,
		SecretTypeDisplayName: capability.DisplayName,
		ProviderSlug:          capability.ProviderSlug,
		PatternCategory:       capability.Category,
		Validity:              string(validity),
		ValidityCheck: secretScanningCapabilityState{
			SupportedByGitHub:   capability.GitHubValidityCheckSupported,
			SupportedByInstance: capability.InstanceValidityCheckSupported,
			Status:              string(validity),
			Description:         capability.ValidityDescription(),
		},
		ProviderNotification: string(providerNotification),
		ProviderNotificationCapability: secretScanningCapabilityState{
			SupportedByGitHub:   capability.GitHubProviderNotificationSupported,
			SupportedByInstance: capability.InstanceProviderNotificationSupported,
			Status:              string(providerNotification),
			Description:         capability.ProviderNotificationDescription(),
		},
		Path:         row.Path,
		Line:         row.LineNo,
		CommitSHA:    row.LastSeenOid,
		FirstSeenSHA: row.FirstSeenOid,
		CreatedAt:    pgTimestampString(row.FirstSeenAt),
		UpdatedAt:    pgTimestampString(row.LastSeenAt),
		ResolvedAt:   pgTimestampString(row.ResolvedAt),
	}
	if base := strings.TrimRight(baseURL, "/"); base != "" {
		out.HTMLURL = base + "/" + owner + "/" + repoName + "/security/secret-scanning"
	}
	return out
}

func secretScanningAlertState(status secretscandb.SecretScanFindingStatus) string {
	switch status {
	case secretscandb.SecretScanFindingStatusOpen:
		return "open"
	case secretscandb.SecretScanFindingStatusResolved, secretscandb.SecretScanFindingStatusAllowlisted, secretscandb.SecretScanFindingStatusStale:
		return "resolved"
	default:
		return string(status)
	}
}

type secretScanningAllowlistResponse struct {
	ID        int64         `json:"id"`
	Pattern   string        `json:"pattern"`
	Path      string        `json:"path"`
	Reason    string        `json:"reason,omitempty"`
	CreatedBy *userEnvelope `json:"created_by,omitempty"`
	CreatedAt string        `json:"created_at"`
}

func presentSecretScanningAllowlist(row secretscandb.SecretScanAllowlist, actors map[int64]*userEnvelope) secretScanningAllowlistResponse {
	var createdBy *userEnvelope
	if row.CreatedBy.Valid {
		createdBy = actors[row.CreatedBy.Int64]
	}
	return secretScanningAllowlistResponse{
		ID:        row.ID,
		Pattern:   row.Pattern,
		Path:      row.Path,
		Reason:    row.Reason,
		CreatedBy: createdBy,
		CreatedAt: pgTimestampString(row.CreatedAt),
	}
}

type secretScanningBypassRequestResponse struct {
	ID            int64         `json:"id"`
	Pattern       string        `json:"pattern"`
	Path          string        `json:"path"`
	CommitSHA     string        `json:"commit_sha"`
	Line          int32         `json:"line"`
	Status        string        `json:"status"`
	RequestedBy   *userEnvelope `json:"requested_by,omitempty"`
	ReviewedBy    *userEnvelope `json:"reviewed_by,omitempty"`
	RequestReason string        `json:"request_reason,omitempty"`
	ReviewNote    string        `json:"review_note,omitempty"`
	ApprovedUntil string        `json:"approved_until,omitempty"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	ReviewedAt    string        `json:"reviewed_at,omitempty"`
	LastSeenAt    string        `json:"last_seen_at"`
}

func presentSecretScanningBypassRequest(row secretscandb.SecretScanBypassRequest, actors map[int64]*userEnvelope) secretScanningBypassRequestResponse {
	var requestedBy, reviewedBy *userEnvelope
	if row.RequestedBy.Valid {
		requestedBy = actors[row.RequestedBy.Int64]
	}
	if row.ReviewedBy.Valid {
		reviewedBy = actors[row.ReviewedBy.Int64]
	}
	return secretScanningBypassRequestResponse{
		ID:            row.ID,
		Pattern:       row.Pattern,
		Path:          row.Path,
		CommitSHA:     row.CommitOid,
		Line:          row.LineNo,
		Status:        string(row.Status),
		RequestedBy:   requestedBy,
		ReviewedBy:    reviewedBy,
		RequestReason: row.RequestReason,
		ReviewNote:    row.ReviewNote,
		ApprovedUntil: pgTimestampString(row.ApprovedUntil),
		CreatedAt:     pgTimestampString(row.CreatedAt),
		UpdatedAt:     pgTimestampString(row.UpdatedAt),
		ReviewedAt:    pgTimestampString(row.ReviewedAt),
		LastSeenAt:    pgTimestampString(row.LastSeenAt),
	}
}

func secretScanningStatusFilter(raw string) (string, error) {
	status := strings.TrimSpace(strings.ToLower(raw))
	switch status {
	case "", string(secretscandb.SecretScanFindingStatusOpen),
		string(secretscandb.SecretScanFindingStatusResolved),
		string(secretscandb.SecretScanFindingStatusAllowlisted),
		string(secretscandb.SecretScanFindingStatusStale):
		return status, nil
	default:
		return "", errInvalidSecretScanningStatus
	}
}

var errInvalidSecretScanningStatus = errors.New("status must be one of open, resolved, allowlisted, or stale")

func allowlistActorIDs(rows []secretscandb.SecretScanAllowlist) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.CreatedBy.Valid {
			ids = append(ids, row.CreatedBy.Int64)
		}
	}
	return ids
}

func bypassActorIDs(rows []secretscandb.SecretScanBypassRequest) []int64 {
	ids := make([]int64, 0, len(rows)*2)
	for _, row := range rows {
		if row.RequestedBy.Valid {
			ids = append(ids, row.RequestedBy.Int64)
		}
		if row.ReviewedBy.Valid {
			ids = append(ids, row.ReviewedBy.Int64)
		}
	}
	return ids
}
