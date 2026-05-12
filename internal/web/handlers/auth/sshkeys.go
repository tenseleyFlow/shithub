// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/sshkey"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// sshKeysList renders /settings/keys with the user's existing keys + a
// blank add form.
func (h *Handlers) sshKeysList(w http.ResponseWriter, r *http.Request) {
	h.renderSSHKeysList(w, r, "", "", "")
}

// renderSSHKeysList is the shared render path for the list page; addError /
// addTitle / addBlob preserve form state when the add path re-renders.
func (h *Handlers) renderSSHKeysList(w http.ResponseWriter, r *http.Request, addError, addTitle, addBlob string) {
	user := middleware.CurrentUserFromContext(r.Context())
	keys, err := h.q.ListUserSSHKeys(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssh: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderPage(w, r, "settings/keys", map[string]any{
		"Title":          "SSH keys",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "keys",
		"Keys":           keys,
		"AddError":       addError,
		"AddTitle":       addTitle,
		"AddBlob":        addBlob,
	})
}

// sshKeysAdd handles POST /settings/keys.
func (h *Handlers) sshKeysAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	title := r.PostFormValue("title")
	blob := r.PostFormValue("public_key")

	parsed, err := sshkey.Parse(title, blob)
	if err != nil {
		h.renderSSHKeysList(w, r, friendlySSHError(err), title, blob)
		return
	}

	// Per-user cap.
	count, err := h.q.CountUserSSHKeys(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssh: count", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= int64(sshkey.MaxKeysPerUser) {
		h.renderSSHKeysList(w, r,
			"You've reached the per-user SSH-key cap. Delete an unused key first.",
			title, blob)
		return
	}

	if _, err := h.q.InsertUserSSHKey(r.Context(), h.d.Pool, usersdb.InsertUserSSHKeyParams{
		UserID:            user.ID,
		Title:             parsed.Title,
		FingerprintSha256: parsed.Fingerprint,
		KeyType:           parsed.Type,
		KeyBits:           int32(parsed.Bits), //nolint:gosec // bits ≤ 8192 in practice; bounded.
		PublicKey:         parsed.PublicKey,
		Kind:              "authentication",
	}); err != nil {
		if isPGUniqueViolation(err) {
			h.renderSSHKeysList(w, r,
				"That key is already registered (here or on another account).",
				title, blob)
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "ssh: insert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionSSHKeyAdded, audit.TargetUser, user.ID, map[string]any{
			"fingerprint": parsed.Fingerprint,
			"type":        parsed.Type,
			"title":       parsed.Title,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "ssh: audit add", "error", err)
	}

	h.notifySSHKeyAdded(r, user.ID, parsed)

	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

// sshKeysDelete handles POST /settings/keys/{id}/delete.
func (h *Handlers) sshKeysDelete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "key not found")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	rows, err := h.q.DeleteUserSSHKey(r.Context(), h.d.Pool, usersdb.DeleteUserSSHKeyParams{
		ID: id, UserID: user.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssh: delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if rows == 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "key not found")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionSSHKeyDeleted, audit.TargetUser, user.ID, map[string]any{
			"key_id": id,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "ssh: audit delete", "error", err)
	}

	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

// notifySSHKeyAdded sends the post-add notification. Best-effort.
func (h *Handlers) notifySSHKeyAdded(r *http.Request, userID int64, parsed *sshkey.Parsed) {
	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, userID)
	if err != nil || !user.PrimaryEmailID.Valid {
		return
	}
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return
	}
	msg, err := email.SSHKeyAddedMessage(h.d.Branding, string(em.Email), user.Username,
		parsed.Title, parsed.Fingerprint, clientIP(r))
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "ssh: build email", "error", err)
		return
	}
	if err := h.d.Email.Send(r.Context(), msg); err != nil {
		h.d.Logger.WarnContext(r.Context(), "ssh: send email", "error", err)
	}
}

// friendlySSHError converts our parser's typed errors to UI-renderable strings.
func friendlySSHError(err error) string {
	switch {
	case errors.Is(err, sshkey.ErrTitleEmpty):
		return "Title is required."
	case errors.Is(err, sshkey.ErrTitleTooLong):
		return "Title may be at most 80 characters."
	case errors.Is(err, sshkey.ErrTitleControl):
		return "Title contains control characters."
	case errors.Is(err, sshkey.ErrUnsupportedAlgo):
		return "That key type isn't accepted. Use ed25519, ECDSA (NIST), or RSA ≥ 2048 bits."
	case errors.Is(err, sshkey.ErrRSATooShort):
		return "RSA keys must be at least 2048 bits."
	case errors.Is(err, sshkey.ErrUnparseable):
		return "We couldn't parse that key. Paste the contents of your .pub file."
	default:
		return "Could not add key."
	}
}

// isPGUniqueViolation matches Postgres SQLSTATE 23505 (unique violation),
// distinguishing duplicate fingerprints from generic insert failures.
func isPGUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
