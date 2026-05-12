// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/devicecode"
)

// MountDeviceCodeAPI registers the RFC 8628 JSON endpoints. Caller is
// responsible for placing r inside a CSRF-exempt group — these are
// non-browser endpoints invoked by CLI / native clients.
func (h *Handlers) MountDeviceCodeAPI(r chi.Router) {
	r.Post("/login/device/code", h.deviceCodeIssue)
	r.Post("/login/oauth/access_token", h.deviceCodeExchange)
}

type deviceCodeIssueResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func (h *Handlers) deviceCodeIssue(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	clientID := strings.TrimSpace(r.PostForm.Get("client_id"))
	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	scope := r.PostForm.Get("scope")

	auth, err := devicecode.Create(r.Context(), devicecode.Deps{Pool: h.d.Pool}, h.d.DeviceCode, clientID, scope)
	if err != nil {
		writeDeviceCodeError(w, err)
		return
	}

	verifyBase := strings.TrimRight(h.d.Branding.BaseURL, "/") + "/login/device"
	verifyComplete := verifyBase + "?user_code=" + url.QueryEscape(auth.UserCode)
	writeJSON(w, http.StatusOK, deviceCodeIssueResponse{
		DeviceCode:              auth.DeviceCode,
		UserCode:                auth.UserCode,
		VerificationURI:         verifyBase,
		VerificationURIComplete: verifyComplete,
		ExpiresIn:               int(auth.ExpiresIn.Seconds()),
		Interval:                int(auth.PollInterval.Seconds()),
	})
}

type deviceCodeExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

func (h *Handlers) deviceCodeExchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	clientID := strings.TrimSpace(r.PostForm.Get("client_id"))
	deviceCode := strings.TrimSpace(r.PostForm.Get("device_code"))
	grantType := r.PostForm.Get("grant_type")
	const wantGrant = "urn:ietf:params:oauth:grant-type:device_code"
	if grantType != wantGrant {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "expected "+wantGrant)
		return
	}
	if clientID == "" || deviceCode == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id and device_code are required")
		return
	}

	res, err := devicecode.Exchange(r.Context(), devicecode.Deps{Pool: h.d.Pool}, clientID, deviceCode, deviceCodeTokenName(r))
	if err != nil {
		writeDeviceCodeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deviceCodeExchangeResponse{
		AccessToken: res.AccessToken,
		TokenType:   res.TokenType,
		Scope:       strings.Join(res.Scopes, ","),
	})
}

// deviceCodeTokenName builds a recognisable name for the PAT minted on
// behalf of the CLI so the user sees it on their /settings/tokens page.
// We don't expose the device's own name (it's not in the protocol), so
// the User-Agent is the only viable hint.
func deviceCodeTokenName(r *http.Request) string {
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	if ua == "" {
		return "device-code"
	}
	if len(ua) > 64 {
		ua = ua[:64]
	}
	return "device-code: " + ua
}

// writeDeviceCodeError maps a devicecode package error to the RFC 8628
// JSON shape and HTTP status. Unknown errors are surfaced as 500
// `server_error`; the package-level sentinels cover every well-formed
// caller path.
func writeDeviceCodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, devicecode.ErrUnauthorizedClient):
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "client_id not allowed")
	case errors.Is(err, devicecode.ErrInvalidScope):
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "unknown scope")
	case errors.Is(err, devicecode.ErrInvalidGrant):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "device_code unknown or already exchanged")
	case errors.Is(err, devicecode.ErrAuthorizationPending):
		writeOAuthError(w, http.StatusBadRequest, "authorization_pending", "user has not approved yet")
	case errors.Is(err, devicecode.ErrSlowDown):
		writeOAuthError(w, http.StatusBadRequest, "slow_down", "increase polling interval")
	case errors.Is(err, devicecode.ErrAccessDenied):
		writeOAuthError(w, http.StatusBadRequest, "access_denied", "user denied the request")
	case errors.Is(err, devicecode.ErrExpiredToken):
		writeOAuthError(w, http.StatusBadRequest, "expired_token", "device_code expired")
	default:
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(oauthError{Error: code, ErrorDescription: desc})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
