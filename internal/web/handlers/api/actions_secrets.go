// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsSecrets registers the S50 §13 part 3 secrets REST
// surface (repo scope + org scope) plus the gh-shaped sealed-box
// public-key probe.
//
//	GET    /api/v1/repos/{o}/{r}/actions/secrets/public-key
//	GET    /api/v1/repos/{o}/{r}/actions/secrets
//	GET    /api/v1/repos/{o}/{r}/actions/secrets/{name}
//	PUT    /api/v1/repos/{o}/{r}/actions/secrets/{name}
//	DELETE /api/v1/repos/{o}/{r}/actions/secrets/{name}
//	GET    /api/v1/orgs/{org}/actions/secrets/public-key
//	GET    /api/v1/orgs/{org}/actions/secrets
//	GET    /api/v1/orgs/{org}/actions/secrets/{name}
//	PUT    /api/v1/orgs/{org}/actions/secrets/{name}
//	DELETE /api/v1/orgs/{org}/actions/secrets/{name}
//
// Scopes: `repo:read` for the repo GETs, `repo:write` for repo
// mutations. Org variants use `repo:read`/`repo:write` likewise —
// the policy gate inside resolveAPIOrg + the org-write check
// constrains who can act.
func (h *Handlers) mountActionsSecrets(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/secrets/public-key", h.actionsSecretsPublicKeyRepo)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/secrets", h.actionsSecretsListRepo)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/secrets/{name}", h.actionsSecretsGetRepo)
		r.Get("/api/v1/orgs/{org}/actions/secrets/public-key", h.actionsSecretsPublicKeyOrg)
		r.Get("/api/v1/orgs/{org}/actions/secrets", h.actionsSecretsListOrg)
		r.Get("/api/v1/orgs/{org}/actions/secrets/{name}", h.actionsSecretsGetOrg)
		// PRO-EXT01-12b: user-scope secrets — read paths use repo:read
		// because the PAT model already gates write surfaces here.
		r.Get("/api/v1/user/actions/secrets/public-key", h.actionsSecretsPublicKeyUser)
		r.Get("/api/v1/user/actions/secrets", h.actionsSecretsListUser)
		r.Get("/api/v1/user/actions/secrets/{name}", h.actionsSecretsGetUser)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/actions/secrets/{name}", h.actionsSecretsPutRepo)
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/secrets/{name}", h.actionsSecretsDeleteRepo)
		r.Put("/api/v1/orgs/{org}/actions/secrets/{name}", h.actionsSecretsPutOrg)
		r.Delete("/api/v1/orgs/{org}/actions/secrets/{name}", h.actionsSecretsDeleteOrg)
		r.Put("/api/v1/user/actions/secrets/{name}", h.actionsSecretsPutUser)
		r.Delete("/api/v1/user/actions/secrets/{name}", h.actionsSecretsDeleteUser)
	})
}

// ─── shared types ───────────────────────────────────────────────────

type secretsPublicKeyResponse struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

type secretMetaResponse struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type secretPutRequest struct {
	EncryptedValue string `json:"encrypted_value"`
	KeyID          string `json:"key_id"`
}

func presentSecretMeta(m secrets.Meta) secretMetaResponse {
	return secretMetaResponse{
		Name:      m.Name,
		CreatedAt: m.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt: m.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ─── repo scope ─────────────────────────────────────────────────────

func (h *Handlers) actionsSecretsPublicKeyRepo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead); !ok {
		return
	}
	h.writeSecretsPublicKey(w, r)
}

func (h *Handlers) actionsSecretsListRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rows, err := h.secretsDeps().List(r.Context(), secrets.RepoScope(repo.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list repo secrets", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]secretMetaResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, presentSecretMeta(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsSecretsGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	rows, err := h.secretsDeps().List(r.Context(), secrets.RepoScope(repo.ID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	for _, m := range rows {
		if m.Name == name {
			writeJSON(w, http.StatusOK, presentSecretMeta(m))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "secret not found")
}

func (h *Handlers) actionsSecretsPutRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	plaintext, ok := h.decodeSecretBody(w, r)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.secretsDeps().Set(r.Context(), secrets.RepoScope(repo.ID), chi.URLParam(r, "name"), plaintext, auth.UserID); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsSecretsDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if err := h.secretsDeps().Delete(r.Context(), secrets.RepoScope(repo.ID), chi.URLParam(r, "name")); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── org scope ──────────────────────────────────────────────────────

func (h *Handlers) actionsSecretsPublicKeyOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	h.writeSecretsPublicKey(w, r)
}

func (h *Handlers) actionsSecretsListOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	rows, err := h.secretsDeps().List(r.Context(), secrets.OrgScope(org.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list org secrets", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]secretMetaResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, presentSecretMeta(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsSecretsGetOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	name := chi.URLParam(r, "name")
	rows, err := h.secretsDeps().List(r.Context(), secrets.OrgScope(org.ID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	for _, m := range rows {
		if m.Name == name {
			writeJSON(w, http.StatusOK, presentSecretMeta(m))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "secret not found")
}

func (h *Handlers) actionsSecretsPutOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	if !h.requireOrgFeature(w, r, org, entitlements.FeatureOrgActionsSecrets, "Organization Actions secrets") {
		return
	}
	plaintext, ok := h.decodeSecretBody(w, r)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.secretsDeps().Set(r.Context(), secrets.OrgScope(org.ID), chi.URLParam(r, "name"), plaintext, auth.UserID); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsSecretsDeleteOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	if err := h.secretsDeps().Delete(r.Context(), secrets.OrgScope(org.ID), chi.URLParam(r, "name")); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ────────────────────────────────────────────────────────

// writeSecretsPublicKey emits the sealed-box public key + key_id.
// Anonymous-readable on the repo path because the policy gate has
// already confirmed `ActionRepoRead`; the org variant goes through
// resolveAPIOrg's gate.
func (h *Handlers) writeSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if h.d.SecretsBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "secrets keypair not configured")
		return
	}
	writeJSON(w, http.StatusOK, secretsPublicKeyResponse{
		KeyID: h.d.SecretsBox.KeyID(),
		Key:   h.d.SecretsBox.PublicKeyBase64(),
	})
}

// decodeSecretBody parses the gh-shaped PUT body and returns the
// plaintext (after sealed-box decoding). Maps malformed body / stale
// key_id / decrypt-failure into clean 400/422 responses. Returns
// (nil, false) and writes the response on any error.
func (h *Handlers) decodeSecretBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if h.d.SecretsBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "secrets keypair not configured")
		return nil, false
	}
	var body secretPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return nil, false
	}
	if body.EncryptedValue == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "encrypted_value is required")
		return nil, false
	}
	if body.KeyID != "" && body.KeyID != h.d.SecretsBox.KeyID() {
		writeAPIError(w, http.StatusUnprocessableEntity, "stale key_id; re-fetch /actions/secrets/public-key")
		return nil, false
	}
	plaintext, err := h.d.SecretsBox.OpenAnonymous(body.EncryptedValue)
	if err != nil {
		switch {
		case errors.Is(err, sealbox.ErrCiphertextMalformed):
			writeAPIError(w, http.StatusBadRequest, "encrypted_value not valid base64")
		default:
			writeAPIError(w, http.StatusUnprocessableEntity, "encrypted_value did not decrypt")
		}
		return nil, false
	}
	return plaintext, true
}

func (h *Handlers) secretsDeps() secrets.Deps {
	return secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger}
}

func writeSecretsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "secret not found")
	case errors.Is(err, secrets.ErrInvalidName):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, secrets.ErrEmptyValue):
		writeAPIError(w, http.StatusUnprocessableEntity, "value must be non-empty")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

// ─── user scope (PRO-EXT01-12b) ─────────────────────────────────────

// requireUserPATAuth returns the authenticated user's id, or writes
// the appropriate error and returns 0. The /api/v1/user/* surface
// requires a PAT — anonymous requests have nothing to scope against.
//
// PRO-EXT_SR-01: a PAT bound to a single repo (PRO-EXT01-11b) is
// rejected here because user-scope resources (personal Actions
// secrets, etc.) span every repo the user owns. Honoring a bound
// token on a cross-repo endpoint would silently expand its blast
// radius beyond what the user authorized when they minted it.
func (h *Handlers) requireUserPATAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return 0, false
	}
	if auth.RepoBinding != 0 {
		writeAPIError(w, http.StatusForbidden,
			"this token is bound to a single repo and cannot manage user-scope resources")
		return 0, false
	}
	return auth.UserID, true
}

// userActionsSecretsAPIGate gates the user-scope WRITE path on
// FeatureUserActionsSecrets when the enforce flag is set. Identical
// shape to the web handler's gate. Returns true to allow, false (and
// writes 402-style 403) to deny.
func (h *Handlers) userActionsSecretsAPIGate(ctx context.Context, w http.ResponseWriter, userID int64) bool {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureUserActionsSecrets)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "api: user actions secrets entitlement", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "entitlement check failed")
		return false
	}
	if !decision.Allowed && h.d.BillingEnforce.UserActionsSecrets {
		writeAPIError(w, http.StatusForbidden, "personal Actions secrets require a Pro subscription")
		return false
	}
	return true
}

func (h *Handlers) actionsSecretsPublicKeyUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUserPATAuth(w, r); !ok {
		return
	}
	h.writeSecretsPublicKey(w, r)
}

func (h *Handlers) actionsSecretsListUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	rows, err := h.secretsDeps().List(r.Context(), secrets.UserScope(userID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user secrets", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]secretMetaResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, presentSecretMeta(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsSecretsGetUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	rows, err := h.secretsDeps().List(r.Context(), secrets.UserScope(userID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	for _, m := range rows {
		if m.Name == name {
			writeJSON(w, http.StatusOK, presentSecretMeta(m))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "secret not found")
}

func (h *Handlers) actionsSecretsPutUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	if !h.userActionsSecretsAPIGate(r.Context(), w, userID) {
		return
	}
	plaintext, ok := h.decodeSecretBody(w, r)
	if !ok {
		return
	}
	if err := h.secretsDeps().Set(r.Context(), secrets.UserScope(userID), chi.URLParam(r, "name"), plaintext, userID); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) actionsSecretsDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	if err := h.secretsDeps().Delete(r.Context(), secrets.UserScope(userID), chi.URLParam(r, "name")); err != nil {
		writeSecretsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
