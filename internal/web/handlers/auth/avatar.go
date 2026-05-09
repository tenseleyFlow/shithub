// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/avatars"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// avatarUploadCap is the multipart-parse cap. Slightly larger than the
// per-image cap inside `avatars.Process` to leave room for the form
// envelope without rejecting valid uploads.
const avatarUploadCap = avatars.MaxUploadBytes + 64*1024

// settingsAvatarUpload handles POST /settings/profile/avatar.
//
// On success: writes 3 PNG variants to the object store, points the
// user's avatar_object_key at the largest variant, and redirects back
// to /settings/profile with a flash on the next render.
func (h *Handlers) settingsAvatarUpload(w http.ResponseWriter, r *http.Request) {
	if h.d.ObjectStore == nil {
		h.d.Render.HTTPError(w, r, http.StatusServiceUnavailable, "avatar storage is not configured")
		return
	}
	// Hard cap on the request body BEFORE multipart parsing so a large
	// upload can't soak memory even via the chunked-encoding path.
	r.Body = http.MaxBytesReader(w, r.Body, avatarUploadCap)
	//nolint:gosec // G120 false positive: avatarUploadCap is a constant bound, and MaxBytesReader above is the real cap.
	if err := r.ParseMultipartForm(avatarUploadCap); err != nil {
		h.renderAvatarError(w, r, "Could not parse upload (file too large?).")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("avatar")
	if err != nil {
		h.renderAvatarError(w, r, "Choose a file to upload.")
		return
	}
	defer func() { _ = file.Close() }()

	variants, hash, err := avatars.Process(file)
	if err != nil {
		h.renderAvatarError(w, r, friendlyAvatarError(err))
		return
	}

	user := middleware.CurrentUserFromContext(r.Context())
	prefix := fmt.Sprintf("avatars/%d/%s", user.ID, hash)
	// Largest variant lives at <prefix>.png and is what the public
	// avatar route serves.
	largestKey := prefix + ".png"
	for _, v := range variants {
		key := fmt.Sprintf("%s-%d.png", prefix, v.Size)
		if v.Size == variants[0].Size {
			key = largestKey
		}
		if _, err := h.d.ObjectStore.Put(
			r.Context(), key,
			bytes.NewReader(v.Data),
			storage.PutOpts{ContentType: "image/png", ContentLength: int64(len(v.Data))},
		); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "avatar: put", "error", err, "key", key)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

	if err := h.q.UpdateUserAvatarKey(r.Context(), h.d.Pool, usersdb.UpdateUserAvatarKeyParams{
		ID:              user.ID,
		AvatarObjectKey: pgtype.Text{String: largestKey, Valid: true},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "avatar: db update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, "/settings/profile", http.StatusSeeOther)
}

// settingsAvatarRemove handles POST /settings/profile/avatar/remove.
//
// We clear avatar_object_key on the user but DO NOT delete the stored
// objects. The current key is content-addressed so future uploads land
// at a new path; old objects stop being referenced and a future
// orphan-sweep job (post-MVP) will purge them.
func (h *Handlers) settingsAvatarRemove(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if err := h.q.UpdateUserAvatarKey(r.Context(), h.d.Pool, usersdb.UpdateUserAvatarKeyParams{
		ID:              user.ID,
		AvatarObjectKey: pgtype.Text{Valid: false},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "avatar: db clear", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/profile", http.StatusSeeOther)
}

func (h *Handlers) renderAvatarError(w http.ResponseWriter, r *http.Request, msg string) {
	row, err := h.q.GetUserByID(r.Context(), h.d.Pool, middleware.CurrentUserFromContext(r.Context()).ID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderProfileForm(w, r, profileForm{
		DisplayName: row.DisplayName,
		Bio:         row.Bio,
		Location:    row.Location,
		Website:     row.Website,
		Company:     row.Company,
		Pronouns:    row.Pronouns,
	}, msg, "")
}

func friendlyAvatarError(err error) string {
	switch {
	case errors.Is(err, avatars.ErrTooLarge):
		return "Image is too large (max 5 MB)."
	case errors.Is(err, avatars.ErrUnsupported):
		return "That format isn't accepted. Use PNG, JPEG, or GIF."
	case errors.Is(err, avatars.ErrDecompression):
		return "Image dimensions are too large."
	case errors.Is(err, avatars.ErrDecode):
		return "We couldn't decode that image."
	default:
		return "Could not process upload."
	}
}
