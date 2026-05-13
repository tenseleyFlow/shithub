// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/gpgkey"
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// gpgKeysAddForm renders the dedicated add-key form page at
// /settings/keys/gpg/new. The form is on its own page (mirroring
// gh's /settings/gpg/new) so it has room for the multi-line armored-
// key textarea without crowding the list page.
func (h *Handlers) gpgKeysAddForm(w http.ResponseWriter, r *http.Request) {
	h.renderGPGKeysAdd(w, r, "", "", "")
}

// renderGPGKeysAdd is the shared render path for the add form page.
// addError / addTitle / addBlob preserve form state across the
// re-render on validation failure.
func (h *Handlers) renderGPGKeysAdd(w http.ResponseWriter, r *http.Request, addError, addTitle, addBlob string) {
	h.renderPage(w, r, "settings/keys_gpg_add", map[string]any{
		"Title":          "Add new GPG key",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "keys",
		"AddError":       addError,
		"AddTitle":       addTitle,
		"AddBlob":        addBlob,
	})
}

// gpgKeysAdd handles POST /settings/keys/gpg. Parses the armored
// block, inserts the primary row + subkey rows in a tx, dispatches
// the backfill job (so existing signed commits get retroactively
// stamped), records an audit entry, and sends the notification
// email.
func (h *Handlers) gpgKeysAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	title := r.PostFormValue("title")
	blob := r.PostFormValue("armored_key")

	parsed, err := gpgkey.Parse(title, blob)
	if err != nil {
		h.renderGPGKeysAdd(w, r, friendlyGPGError(err), title, blob)
		return
	}

	// Per-user cap.
	count, err := h.q.CountUserGPGKeys(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "gpg: count", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= int64(gpgkey.MaxKeysPerUser) {
		h.renderGPGKeysAdd(w, r,
			"You've reached the per-user GPG-key cap. Delete an unused key first.",
			title, blob)
		return
	}

	// Insert primary + subkey rows in a single tx. The subkey
	// fingerprint index must be populated atomically with the
	// primary row or a concurrent verification could resolve to a
	// half-inserted state.
	if err := h.insertGPGKeyTx(r.Context(), user.ID, parsed); err != nil {
		if errors.Is(err, errDuplicateFingerprint) {
			h.renderGPGKeysAdd(w, r,
				"That key is already registered (here or on another account).",
				title, blob)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "gpg: insert tx", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionGPGKeyAdded, audit.TargetUser, user.ID, map[string]any{
			"fingerprint": parsed.Fingerprint,
			"key_id":      parsed.KeyID,
			"name":        parsed.Name,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "gpg: audit add", "error", err)
	}

	h.notifyGPGKeyAdded(r, user.ID, parsed)

	// Dispatch the eager backfill. Best-effort: a dispatch failure
	// (e.g. the worker queue is down) shouldn't block the user from
	// seeing their key on the settings page. The bulk
	// shithubd gpg-backfill-all command picks up the slack.
	if err := sigverify.DispatchForKey(r.Context(), h.d.Pool, user.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "gpg: dispatch backfill", "error", err)
	}

	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

// gpgKeysDelete handles POST /settings/keys/gpg/{id}/delete.
// Soft-deletes (stamps revoked_at) so historical commit-verification
// attribution is preserved; the same tx also invalidates any cache
// rows that resolved against this key's subkeys.
func (h *Handlers) gpgKeysDelete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "key not found")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	deleted, err := h.softDeleteGPGKeyTx(r.Context(), id, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "gpg: delete tx", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !deleted {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "key not found")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionGPGKeyDeleted, audit.TargetUser, user.ID, map[string]any{
			"key_id": id,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "gpg: audit delete", "error", err)
	}

	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

// insertGPGKeyTx inserts the primary user_gpg_keys row and one
// user_gpg_subkeys row per subkey atomically. Returns
// errDuplicateFingerprint if the partial unique index on
// fingerprint trips — the caller surfaces that as a friendly
// "already registered" error.
func (h *Handlers) insertGPGKeyTx(ctx context.Context, userID int64, parsed *gpgkey.Parsed) error {
	tx, err := h.d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	subkeysJSON, err := json.Marshal(parsed.Subkeys)
	if err != nil {
		return err
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
		ExpiresAt:         nullableTimestamptz(parsed.ExpiresAt),
	})
	if err != nil {
		if isPGUniqueViolation(err) {
			return errDuplicateFingerprint
		}
		return err
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
			ExpiresAt:         nullableTimestamptz(sk.ExpiresAt),
		}); err != nil {
			if isPGUniqueViolation(err) {
				// A subkey of this primary is already registered
				// (someone else's primary owns a subkey with the
				// same fingerprint). Vanishingly rare, but surface
				// the same friendly error as a primary collision.
				return errDuplicateFingerprint
			}
			return err
		}
	}

	return tx.Commit(ctx)
}

// softDeleteGPGKeyTx soft-deletes the key + all its subkeys + stamps
// invalidated_at on dependent verification cache rows. All three
// happen atomically so the cache and keyring stay in sync.
func (h *Handlers) softDeleteGPGKeyTx(ctx context.Context, id, userID int64) (bool, error) {
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

	// Invalidate dependent cache rows for each subkey. Done via the
	// reposdb query (commit_verification_cache lives in the repos
	// domain even though subkeys live in users — the FK reference is
	// what binds them).
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

// notifyGPGKeyAdded sends the post-add notification. Best-effort —
// any failure logs but doesn't fail the request.
func (h *Handlers) notifyGPGKeyAdded(r *http.Request, userID int64, parsed *gpgkey.Parsed) {
	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, userID)
	if err != nil || !user.PrimaryEmailID.Valid {
		return
	}
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return
	}
	msg, err := email.GPGKeyAddedMessage(h.d.Branding, string(em.Email), user.Username,
		parsed.Name, parsed.Fingerprint, clientIP(r))
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "gpg: build email", "error", err)
		return
	}
	if err := h.d.Email.Send(r.Context(), msg); err != nil {
		h.d.Logger.WarnContext(r.Context(), "gpg: send email", "error", err)
	}
}

// errDuplicateFingerprint is the sentinel returned from
// insertGPGKeyTx when the partial unique index on fingerprint
// trips. The caller surfaces this as a friendly error rather than a
// generic 500.
var errDuplicateFingerprint = errors.New("gpgkeys: duplicate fingerprint")

// nullableTimestamptz converts an optional *time.Time (used by the
// parser to indicate "this key never expires" via nil) into the
// pgtype shape sqlc requires.
func nullableTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// friendlyGPGError translates the gpgkey parser's sentinel errors
// into UI-renderable strings. Keeps the precise distinctions our
// parser draws (private-key block, signature block, expired,
// encryption-only, RSA-too-short) visible to the user — gh's
// "We got an error" generic banner is less helpful.
func friendlyGPGError(err error) string {
	switch {
	case errors.Is(err, gpgkey.ErrPrivateKeyBlock):
		return "That looks like a private key — please upload your public key " +
			"(gpg --armor --export <id>)."
	case errors.Is(err, gpgkey.ErrSignatureBlock):
		return "That looks like a signature, not a public key."
	case errors.Is(err, gpgkey.ErrUnparseable):
		return "We couldn't parse that key. Please paste a public key " +
			"armored block starting with -----BEGIN PGP PUBLIC KEY BLOCK-----."
	case errors.Is(err, gpgkey.ErrNoIdentities):
		return "That key has no user IDs."
	case errors.Is(err, gpgkey.ErrExpired):
		return "That key has expired."
	case errors.Is(err, gpgkey.ErrUnsupportedAlgo):
		return "That key algorithm isn't accepted. Use ed25519, " +
			"ECDSA (NIST), or RSA ≥ 2048 bits."
	case errors.Is(err, gpgkey.ErrRSATooShort):
		return "RSA keys must be at least 2048 bits."
	case errors.Is(err, gpgkey.ErrMultipleEntities):
		return "Please upload one public key at a time."
	case errors.Is(err, gpgkey.ErrNameTooLong):
		return "Name may be at most 80 characters."
	case errors.Is(err, gpgkey.ErrNameControl):
		return "Name contains control characters."
	default:
		return "Could not add key."
	}
}
