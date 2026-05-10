// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/avatars"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const avatarUploadCap = avatars.MaxUploadBytes + 64*1024

func (h *Handlers) settingsProfile(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	h.renderSettingsProfile(w, r, org, "", "")
}

func (h *Handlers) settingsAvatarUpload(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if h.d.ObjectStore == nil {
		h.d.Render.HTTPError(w, r, http.StatusServiceUnavailable, "avatar storage is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, avatarUploadCap)
	//nolint:gosec // G120 false positive: avatarUploadCap is constant-bounded, MaxBytesReader enforces the cap.
	if err := r.ParseMultipartForm(avatarUploadCap); err != nil {
		h.renderSettingsProfile(w, r, org, friendlyAvatarErr(err), "")
		return
	}
	file, _, err := r.FormFile("avatar")
	if err != nil {
		h.renderSettingsProfile(w, r, org, "Choose an image file to upload.", "")
		return
	}
	defer func() { _ = file.Close() }()

	variants, hash, err := avatars.Process(file)
	if err != nil {
		h.renderSettingsProfile(w, r, org, friendlyAvatarErr(err), "")
		return
	}
	prefix := fmt.Sprintf("avatars/orgs/%d/%s", org.ID, hash)
	largest := ""
	for _, v := range variants {
		key := fmt.Sprintf("%s-%d.png", prefix, v.Size)
		if v.Size == variants[0].Size {
			key = prefix + ".png"
			largest = key
		}
		if _, err := h.d.ObjectStore.Put(
			r.Context(), key,
			bytes.NewReader(v.Data),
			storage.PutOpts{ContentType: "image/png", ContentLength: int64(len(v.Data))},
		); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "org avatar: put", "error", err, "key", key)
			h.renderSettingsProfile(w, r, org, "Could not store that avatar. Try again.", "")
			return
		}
	}
	if err := orgsdb.New().SetOrgAvatarKey(r.Context(), h.d.Pool, orgsdb.SetOrgAvatarKeyParams{
		ID:              org.ID,
		AvatarObjectKey: pgtype.Text{String: largest, Valid: true},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org avatar: db update", "error", err)
		h.renderSettingsProfile(w, r, org, "Could not save that avatar. Try again.", "")
		return
	}
	http.Redirect(w, r, orgSettingsProfilePath(org), http.StatusSeeOther)
}

func (h *Handlers) settingsAvatarRemove(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := orgsdb.New().SetOrgAvatarKey(r.Context(), h.d.Pool, orgsdb.SetOrgAvatarKeyParams{
		ID:              org.ID,
		AvatarObjectKey: pgtype.Text{},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org avatar: db clear", "error", err)
		h.renderSettingsProfile(w, r, org, "Could not remove that avatar. Try again.", "")
		return
	}
	http.Redirect(w, r, orgSettingsProfilePath(org), http.StatusSeeOther)
}

func (h *Handlers) renderSettingsProfile(
	w http.ResponseWriter,
	r *http.Request,
	org orgsdb.Org,
	errMsg string,
	success string,
) {
	h.renderSettingsProfileWithForm(w, r, org, settingsProfileFormFromOrg(org), errMsg, success)
}

func orgSettingsProfilePath(org orgsdb.Org) string {
	return "/organizations/" + org.Slug + "/settings/profile"
}

func friendlyAvatarErr(err error) string {
	switch {
	case errors.Is(err, avatars.ErrTooLarge):
		return "Avatar must be 5 MB or smaller."
	case errors.Is(err, avatars.ErrUnsupported):
		return "Avatar must be a PNG, JPEG, or GIF image."
	case errors.Is(err, avatars.ErrDecompression):
		return "Avatar dimensions are too large."
	case errors.Is(err, avatars.ErrDecode):
		return "Could not decode that image."
	default:
		return "Could not upload that avatar."
	}
}
