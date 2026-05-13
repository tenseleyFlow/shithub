// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/sshkey"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// sshKeysList renders the combined /settings/keys page (both SSH and
// GPG sections) with the user's existing keys + a blank SSH add form.
// The sidebar entry was renamed to "SSH and GPG keys" in S51 to match
// gh's pattern of bundling both surfaces on one settings page.
func (h *Handlers) sshKeysList(w http.ResponseWriter, r *http.Request) {
	h.renderKeysList(w, r, "", "", "")
}

// renderKeysList is the shared render path for the combined SSH+GPG
// list page. addError / addTitle / addBlob preserve SSH-side form
// state when the SSH add path re-renders; the GPG add path renders a
// separate page (settings/keys_gpg_add) so its form state lives there.
func (h *Handlers) renderKeysList(w http.ResponseWriter, r *http.Request, addError, addTitle, addBlob string) {
	user := middleware.CurrentUserFromContext(r.Context())
	sshKeys, err := h.q.ListUserSSHKeys(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "ssh: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	gpgKeys, err := h.q.ListUserGPGKeys(r.Context(), h.d.Pool, usersdb.ListUserGPGKeysParams{
		UserID: user.ID,
		Limit:  int32(gpgListPageSize), //nolint:gosec // bounded constant
		Offset: 0,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "gpg: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderPage(w, r, "settings/keys", map[string]any{
		"Title":          "SSH and GPG keys",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "keys",
		"Keys":           sshKeys,
		"GPGKeys":        viewModelGPGKeys(gpgKeys),
		"AddError":       addError,
		"AddTitle":       addTitle,
		"AddBlob":        addBlob,
	})
}

// gpgListPageSize is the SHOULD-be-enough cap for the per-user GPG
// key count in the settings UI. Mirrors the parser's MaxKeysPerUser;
// the list is short by definition.
const gpgListPageSize = 100

// gpgKeyView is the template-facing shape of a GPG key row. Decoupled
// from the sqlc UserGpgKey row so the template doesn't have to know
// about pgtype wrappers or unmarshal subkeys JSON inline.
type gpgKeyView struct {
	ID        int64
	Name      string
	KeyID     string
	Emails    []struct{ Email string }
	SubkeyIDs []string
	CreatedAt time.Time
}

// viewModelGPGKeys transforms the sqlc row shape into the template-
// facing struct. Unmarshals the subkeys JSONB once per row so the
// template can render the comma-separated subkey-IDs line without
// re-parsing.
func viewModelGPGKeys(rows []usersdb.UserGpgKey) []gpgKeyView {
	views := make([]gpgKeyView, 0, len(rows))
	for _, row := range rows {
		view := gpgKeyView{
			ID:        row.ID,
			Name:      row.Name,
			KeyID:     strings.ToUpper(row.KeyID),
			CreatedAt: row.CreatedAt.Time,
		}
		for _, uid := range row.Uids {
			view.Emails = append(view.Emails, struct{ Email string }{Email: uid})
		}
		var subkeys []struct {
			KeyID string `json:"KeyID"`
		}
		_ = json.Unmarshal(row.Subkeys, &subkeys)
		for _, sk := range subkeys {
			view.SubkeyIDs = append(view.SubkeyIDs, strings.ToUpper(sk.KeyID))
		}
		views = append(views, view)
	}
	return views
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
		h.renderKeysList(w, r, friendlySSHError(err), title, blob)
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
		h.renderKeysList(w, r,
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
			h.renderKeysList(w, r,
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
