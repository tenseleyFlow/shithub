// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/gpgkey"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountGPGKeys registers the OpenPGP keys REST surface. JSON shape
// mirrors GitHub's /user/gpg_keys EXACTLY — every field name and
// nullability matches docs.github.com/en/rest/users/gpg-keys so the
// shithub-cli client can consume responses without per-field shims.
//
//	GET    /api/v1/user/gpg_keys        list (paginated)
//	POST   /api/v1/user/gpg_keys        add { name, armored_public_key }
//	GET    /api/v1/user/gpg_keys/{id}   get one
//	DELETE /api/v1/user/gpg_keys/{id}   soft-delete
//
// Scopes: user:read for GETs, user:write for POST/DELETE.
func (h *Handlers) mountGPGKeys(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/gpg_keys", h.gpgKeysList)
		r.Get("/api/v1/user/gpg_keys/{id}", h.gpgKeyGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Post("/api/v1/user/gpg_keys", h.gpgKeyCreate)
		r.Delete("/api/v1/user/gpg_keys/{id}", h.gpgKeyDelete)
	})
}

// gpgKeyResponse mirrors GitHub's GpgKey type byte-for-byte. Two
// fields worth calling out as gh-exact:
//
//   - can_encrypt_comms + can_encrypt_storage are split per RFC 4880
//     §5.2.3.21. gh exposes both bits; consumers expect both.
//   - public_key and raw_key both carry the armored block. gh
//     historically distinguishes them; we emit the same armored text
//     in both for maximum client compatibility.
//
// can_authenticate is NOT in gh's response — OpenPGP carries the bit
// but gh doesn't surface it. We follow suit; the DB column stays in
// case S52/S53 want to expose it.
type gpgKeyResponse struct {
	ID                int64               `json:"id"`
	Name              string              `json:"name"`
	PrimaryKeyID      *int64              `json:"primary_key_id"`
	KeyID             string              `json:"key_id"`
	PublicKey         string              `json:"public_key"`
	Emails            []gpgEmailResponse  `json:"emails"`
	Subkeys           []gpgSubkeyResponse `json:"subkeys"`
	CanSign           bool                `json:"can_sign"`
	CanEncryptComms   bool                `json:"can_encrypt_comms"`
	CanEncryptStorage bool                `json:"can_encrypt_storage"`
	CanCertify        bool                `json:"can_certify"`
	CreatedAt         string              `json:"created_at"`
	ExpiresAt         *string             `json:"expires_at"`
	Revoked           bool                `json:"revoked"`
	RawKey            string              `json:"raw_key"`
}

type gpgEmailResponse struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
}

type gpgSubkeyResponse struct {
	ID                int64               `json:"id"`
	PrimaryKeyID      int64               `json:"primary_key_id"`
	KeyID             string              `json:"key_id"`
	PublicKey         string              `json:"public_key"`
	Emails            []gpgEmailResponse  `json:"emails"`
	Subkeys           []gpgSubkeyResponse `json:"subkeys"`
	CanSign           bool                `json:"can_sign"`
	CanEncryptComms   bool                `json:"can_encrypt_comms"`
	CanEncryptStorage bool                `json:"can_encrypt_storage"`
	CanCertify        bool                `json:"can_certify"`
	CreatedAt         string              `json:"created_at"`
	ExpiresAt         *string             `json:"expires_at"`
	RawKey            *string             `json:"raw_key"`
	Revoked           bool                `json:"revoked"`
}

// presentGPGKey transforms a sqlc row + the user's verified-email
// set into the wire response. The verified-emails parameter is the
// expensive lookup; the caller does it once per request and passes
// it through so we don't re-query inside the loop.
func presentGPGKey(k usersdb.UserGpgKey, verifiedEmails map[string]bool) gpgKeyResponse {
	resp := gpgKeyResponse{
		ID:                k.ID,
		Name:              k.Name,
		KeyID:             strings.ToUpper(k.KeyID),
		PublicKey:         k.Armored,
		RawKey:            k.Armored,
		CanSign:           k.CanSign,
		CanEncryptComms:   k.CanEncryptComms,
		CanEncryptStorage: k.CanEncryptStorage,
		CanCertify:        k.CanCertify,
		CreatedAt:         k.CreatedAt.Time.UTC().Format(time.RFC3339),
		Revoked:           k.RevokedAt.Valid,
		Emails:            []gpgEmailResponse{},
		Subkeys:           []gpgSubkeyResponse{},
	}
	if k.ExpiresAt.Valid {
		exp := k.ExpiresAt.Time.UTC().Format(time.RFC3339)
		resp.ExpiresAt = &exp
	}
	for _, uid := range k.Uids {
		resp.Emails = append(resp.Emails, gpgEmailResponse{
			Email:    uid,
			Verified: verifiedEmails[strings.ToLower(uid)],
		})
	}

	// Subkeys are stored as JSONB at upload time in the gpgkey
	// parser's ParsedSubkey shape; transform here to the gh-exact
	// nested shape. Best-effort: a bad JSONB blob (corrupted at
	// rest) skips the subkeys array, leaving it empty rather than
	// failing the whole response.
	var parsedSubkeys []struct {
		Fingerprint       string     `json:"Fingerprint"`
		KeyID             string     `json:"KeyID"`
		CanSign           bool       `json:"CanSign"`
		CanEncryptComms   bool       `json:"CanEncryptComms"`
		CanEncryptStorage bool       `json:"CanEncryptStorage"`
		CanCertify        bool       `json:"CanCertify"`
		ExpiresAt         *time.Time `json:"ExpiresAt"`
	}
	_ = json.Unmarshal(k.Subkeys, &parsedSubkeys)
	for _, sk := range parsedSubkeys {
		sub := gpgSubkeyResponse{
			PrimaryKeyID:      k.ID,
			KeyID:             strings.ToUpper(sk.KeyID),
			PublicKey:         "",
			Emails:            []gpgEmailResponse{},
			Subkeys:           []gpgSubkeyResponse{},
			CanSign:           sk.CanSign,
			CanEncryptComms:   sk.CanEncryptComms,
			CanEncryptStorage: sk.CanEncryptStorage,
			CanCertify:        sk.CanCertify,
			CreatedAt:         k.CreatedAt.Time.UTC().Format(time.RFC3339),
			Revoked:           false,
		}
		if sk.ExpiresAt != nil {
			exp := sk.ExpiresAt.UTC().Format(time.RFC3339)
			sub.ExpiresAt = &exp
		}
		resp.Subkeys = append(resp.Subkeys, sub)
	}
	return resp
}

// loadVerifiedEmails returns a lowercase-keyed set of the user's
// verified-email addresses for the emails[].verified cross-check.
// One DB hit per request; the caller threads the result through
// presentGPGKey to avoid re-querying per row.
func (h *Handlers) loadVerifiedEmails(ctx context.Context, userID int64) (map[string]bool, error) {
	rows, err := h.q.ListUserEmailsForUser(ctx, h.d.Pool, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[strings.ToLower(string(row.Email))] = row.Verified
	}
	return out, nil
}

func (h *Handlers) gpgKeysList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	total, err := h.q.CountUserGPGKeys(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count gpg keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := h.q.ListUserGPGKeys(r.Context(), h.d.Pool, usersdb.ListUserGPGKeysParams{
		UserID: auth.UserID,
		Limit:  int32(perPage), //nolint:gosec // bounded by apipage.MaxPerPage
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list gpg keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	verified, err := h.loadVerifiedEmails(r.Context(), auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: load user emails for gpg list", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	link := apipage.Page{
		Current: page, PerPage: perPage, Total: int(total),
	}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]gpgKeyResponse, 0, len(rows))
	for _, k := range rows {
		out = append(out, presentGPGKey(k, verified))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) gpgKeyGet(w http.ResponseWriter, r *http.Request) {
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
	k, err := h.q.GetUserGPGKey(r.Context(), h.d.Pool, usersdb.GetUserGPGKeyParams{
		ID: id, UserID: auth.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "key not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get gpg key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	verified, err := h.loadVerifiedEmails(r.Context(), auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: load user emails for gpg get", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, presentGPGKey(k, verified))
}

type gpgKeyCreateRequest struct {
	Name             string `json:"name"`
	ArmoredPublicKey string `json:"armored_public_key"`
}

func (h *Handlers) gpgKeyCreate(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body gpgKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	parsed, err := gpgkey.Parse(body.Name, body.ArmoredPublicKey)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, gpgKeyAPIErrorMessage(err))
		return
	}
	count, err := h.q.CountUserGPGKeys(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count gpg keys", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if count >= int64(gpgkey.MaxKeysPerUser) {
		writeAPIError(w, http.StatusUnprocessableEntity, "per-user GPG-key cap reached")
		return
	}
	row, err := h.insertGPGKeyTx(r.Context(), auth.UserID, parsed)
	if err != nil {
		if errors.Is(err, errAPIDuplicateGPGFingerprint) {
			writeAPIError(w, http.StatusUnprocessableEntity, "key already registered")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: insert gpg key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}

	// Audit + backfill dispatch are best-effort; failures don't
	// undo the insert, but they DO get logged. The nil-check guards
	// the test router (Deps without an Audit recorder) — production
	// always supplies one.
	if h.d.Audit != nil {
		if err := h.d.Audit.Record(r.Context(), h.d.Pool, auth.UserID,
			audit.ActionGPGKeyAdded, audit.TargetUser, auth.UserID, map[string]any{
				"fingerprint": parsed.Fingerprint,
				"key_id":      parsed.KeyID,
				"name":        parsed.Name,
			}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "api: audit gpg add", "error", err)
		}
	}
	if err := sigverify.DispatchForKey(r.Context(), h.d.Pool, auth.UserID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "api: dispatch gpg backfill", "error", err)
	}

	verified, err := h.loadVerifiedEmails(r.Context(), auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: load user emails after create", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, presentGPGKey(row, verified))
}

func (h *Handlers) gpgKeyDelete(w http.ResponseWriter, r *http.Request) {
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
	deleted, err := h.softDeleteGPGKeyTx(r.Context(), id, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete gpg key", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if !deleted {
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	if h.d.Audit != nil {
		if err := h.d.Audit.Record(r.Context(), h.d.Pool, auth.UserID,
			audit.ActionGPGKeyDeleted, audit.TargetUser, auth.UserID, map[string]any{
				"key_id": id,
			}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "api: audit gpg delete", "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// gpgKeyAPIErrorMessage maps the gpgkey parser sentinels to
// API-client-appropriate strings (terse, no UI prose).
func gpgKeyAPIErrorMessage(err error) string {
	switch {
	case errors.Is(err, gpgkey.ErrPrivateKeyBlock):
		return "uploaded block is a private key, not a public key"
	case errors.Is(err, gpgkey.ErrSignatureBlock):
		return "uploaded block is a signature, not a public key"
	case errors.Is(err, gpgkey.ErrUnparseable):
		return "could not parse armored public key block"
	case errors.Is(err, gpgkey.ErrNoIdentities):
		return "key has no user IDs"
	case errors.Is(err, gpgkey.ErrExpired):
		return "key has expired"
	case errors.Is(err, gpgkey.ErrUnsupportedAlgo):
		return "unsupported key algorithm"
	case errors.Is(err, gpgkey.ErrRSATooShort):
		return "RSA keys must be at least 2048 bits"
	case errors.Is(err, gpgkey.ErrMultipleEntities):
		return "upload one public key at a time"
	case errors.Is(err, gpgkey.ErrNameTooLong):
		return "name must be at most 80 characters"
	case errors.Is(err, gpgkey.ErrNameControl):
		return "name contains control characters"
	default:
		return "invalid key"
	}
}

// insertGPGKeyTx duplicates the same primary+subkeys atomic-insert
// logic the HTML handler uses, scoped here so the api package
// doesn't have to import the auth handler package.
func (h *Handlers) insertGPGKeyTx(ctx context.Context, userID int64, parsed *gpgkey.Parsed) (usersdb.UserGpgKey, error) {
	tx, err := h.d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return usersdb.UserGpgKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	subkeysJSON, err := json.Marshal(parsed.Subkeys)
	if err != nil {
		return usersdb.UserGpgKey{}, err
	}
	row, err := h.q.InsertUserGPGKey(ctx, tx, usersdb.InsertUserGPGKeyParams{
		UserID:            userID,
		Name:              parsed.Name,
		Fingerprint:       parsed.Fingerprint,
		KeyID:             parsed.KeyID,
		Armored:           parsed.Armored,
		CanSign:           parsed.CanSign,
		CanEncryptComms:   parsed.CanEncryptComms,
		CanEncryptStorage: parsed.CanEncryptStorage,
		CanCertify:        parsed.CanCertify,
		CanAuthenticate:   parsed.CanAuthenticate,
		Uids:              parsed.UIDs,
		Subkeys:           subkeysJSON,
		PrimaryAlgo:       parsed.PrimaryAlgo,
		ExpiresAt:         apiNullableTimestamptz(parsed.ExpiresAt),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return usersdb.UserGpgKey{}, errAPIDuplicateGPGFingerprint
		}
		return usersdb.UserGpgKey{}, err
	}
	for _, sk := range parsed.Subkeys {
		if _, err := h.q.InsertUserGPGSubkey(ctx, tx, usersdb.InsertUserGPGSubkeyParams{
			GpgKeyID:          row.ID,
			Fingerprint:       sk.Fingerprint,
			KeyID:             sk.KeyID,
			CanSign:           sk.CanSign,
			CanEncryptComms:   sk.CanEncryptComms,
			CanEncryptStorage: sk.CanEncryptStorage,
			CanCertify:        sk.CanCertify,
			ExpiresAt:         apiNullableTimestamptz(sk.ExpiresAt),
		}); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return usersdb.UserGpgKey{}, errAPIDuplicateGPGFingerprint
			}
			return usersdb.UserGpgKey{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return usersdb.UserGpgKey{}, err
	}
	return row, nil
}

// softDeleteGPGKeyTx mirrors the HTML handler's soft-delete +
// cache-invalidation transaction.
func (h *Handlers) softDeleteGPGKeyTx(ctx context.Context, id, userID int64) (bool, error) {
	// Defer to the shared reposdb invalidation query. We don't want
	// the api handler to import the auth handler package, so the
	// query call is inlined here.
	tx, err := h.d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := h.q.SoftDeleteUserGPGKey(ctx, tx, usersdb.SoftDeleteUserGPGKeyParams{
		ID:     id,
		UserID: userID,
	})
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}
	subkeys, err := h.q.ListSubkeysForGPGKey(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if err := h.q.SoftDeleteSubkeysForGPGKey(ctx, tx, id); err != nil {
		return false, err
	}
	rq := reposdb.New()
	for _, sk := range subkeys {
		if err := rq.InvalidateVerificationsForSubkey(ctx, tx, pgtype.Int8{Int64: sk.ID, Valid: true}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// errAPIDuplicateGPGFingerprint is the sentinel for the partial
// unique index violation. Surfaced as 422 "key already registered".
var errAPIDuplicateGPGFingerprint = errors.New("api: duplicate gpg fingerprint")

// apiNullableTimestamptz mirrors the auth handler's helper. Lives
// here as a private symbol so the api package doesn't take a hard
// dependency on the auth handler package.
func apiNullableTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
