// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/sshkey"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountUserKeys registers the SSH-keys REST surface for the authenticated
// user. Shape mirrors GitHub's /user/keys (authentication keys only;
// signing keys land at /user/ssh_signing_keys in a future batch).
//
//	GET    /api/v1/user/keys        list (paginated)
//	POST   /api/v1/user/keys        add { title, key }
//	GET    /api/v1/user/keys/{id}   get one
//	DELETE /api/v1/user/keys/{id}   remove
//
// Scopes: user:read for GETs, user:write for POST/DELETE.
func (h *Handlers) mountUserKeys(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/keys", h.userKeysList)
		r.Get("/api/v1/user/keys/{id}", h.userKeyGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Post("/api/v1/user/keys", h.userKeyCreate)
		r.Delete("/api/v1/user/keys/{id}", h.userKeyDelete)
	})
}

type userKeyResponse struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	Verified    bool   `json:"verified"`
	ReadOnly    bool   `json:"read_only"`
	CreatedAt   string `json:"created_at"`
}

func presentUserKey(k usersdb.UserSshKey) userKeyResponse {
	return userKeyResponse{
		ID:          k.ID,
		Title:       k.Title,
		Key:         k.PublicKey,
		Fingerprint: presentSSHFingerprint(k.FingerprintSha256),
		KeyType:     k.KeyType,
		// Every key shithub stores has been parsed and validated at
		// upload time. Surface as verified=true so gh-shaped clients
		// that key off this field continue to work.
		Verified:  true,
		ReadOnly:  false,
		CreatedAt: k.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

func presentSSHFingerprint(fp string) string {
	if strings.HasPrefix(fp, "SHA256:") {
		return fp
	}
	return "SHA256:" + fp
}

func (h *Handlers) userKeysList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total, err := h.q.CountUserSSHKeysByKind(r.Context(), h.d.Pool, usersdb.CountUserSSHKeysByKindParams{
		UserID: auth.UserID, Kind: "authentication",
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count user keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := h.q.ListUserSSHKeysByKind(r.Context(), h.d.Pool, usersdb.ListUserSSHKeysByKindParams{
		UserID: auth.UserID,
		Kind:   "authentication",
		Limit:  int32(perPage),
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	link := apipage.Page{
		Current: page, PerPage: perPage, Total: int(total),
	}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]userKeyResponse, 0, len(rows))
	for _, k := range rows {
		out = append(out, presentUserKey(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) userKeyGet(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	k, err := h.q.GetUserSSHKey(r.Context(), h.d.Pool, usersdb.GetUserSSHKeyParams{
		ID: id, UserID: auth.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "key not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get user key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if k.Kind != "authentication" {
		// Same shape as a non-existent key: the signing-keys surface
		// has its own route that this endpoint deliberately doesn't
		// expose.
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	writeJSON(w, http.StatusOK, presentUserKey(k))
}

type userKeyCreateRequest struct {
	Title string `json:"title"`
	Key   string `json:"key"`
}

func (h *Handlers) userKeyCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body userKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	parsed, err := sshkey.Parse(body.Title, body.Key)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, sshKeyAPIErrorMessage(err))
		return
	}
	count, err := h.q.CountUserSSHKeys(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count user keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if count >= int64(sshkey.MaxKeysPerUser) {
		writeAPIError(w, http.StatusUnprocessableEntity, "per-user SSH-key cap reached")
		return
	}
	k, err := h.q.InsertUserSSHKey(r.Context(), h.d.Pool, usersdb.InsertUserSSHKeyParams{
		UserID:            auth.UserID,
		Title:             parsed.Title,
		FingerprintSha256: parsed.Fingerprint,
		KeyType:           parsed.Type,
		KeyBits:           int32(parsed.Bits), //nolint:gosec // RSA-bit ceiling is bounded by sshkey.Parse.
		PublicKey:         parsed.PublicKey,
		Kind:              "authentication",
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeAPIError(w, http.StatusUnprocessableEntity, "key already registered")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: insert user key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, presentUserKey(k))
}

func (h *Handlers) userKeyDelete(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	rows, err := h.q.DeleteUserSSHKey(r.Context(), h.d.Pool, usersdb.DeleteUserSSHKeyParams{
		ID: id, UserID: auth.UserID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete user key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if rows == 0 {
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sshKeyAPIErrorMessage maps the typed parser errors to user-facing
// strings appropriate for an API client (no UI verbiage).
func sshKeyAPIErrorMessage(err error) string {
	switch {
	case errors.Is(err, sshkey.ErrTitleEmpty):
		return "title is required"
	case errors.Is(err, sshkey.ErrTitleTooLong):
		return "title must be at most 80 characters"
	case errors.Is(err, sshkey.ErrTitleControl):
		return "title contains control characters"
	case errors.Is(err, sshkey.ErrUnsupportedAlgo):
		return "unsupported key algorithm (use ed25519, ECDSA, or RSA >= 2048 bits)"
	case errors.Is(err, sshkey.ErrRSATooShort):
		return "RSA keys must be at least 2048 bits"
	case errors.Is(err, sshkey.ErrUnparseable):
		return "could not parse key blob"
	default:
		return "invalid key"
	}
}

// sanitizedURL returns a copy of the request URL with no scheme/host,
// suitable for feeding into apipage.LinkHeader without leaking proxy
// internals. The helper exists so handlers don't have to repeat the
// boilerplate copy + clear.
func sanitizedURL(r *http.Request) *url.URL {
	u := *r.URL
	u.Scheme = ""
	u.Host = ""
	return &u
}
