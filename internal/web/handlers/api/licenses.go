// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/repos/templates"
)

// audit-I35 (I6 bundle): expose the curated SPDX license list as a
// discoverable endpoint. Pre-fix users typing `--license mit` against
// the gh-style lowercase keys hit "unknown license" with no path to
// the canonical list — gh's CLI consults `GET /licenses` and accepts
// any casing the catalog covers. We mirror the contract.
//
// The endpoint is anonymous-friendly (curated metadata, no per-user
// state) and short — fits inside the existing PAT-gated mount group
// because there's no value gating it behind auth.

// mountLicenses wires the /api/v1/licenses surface.
//
//	GET /api/v1/licenses           lowercase-key catalog
//	GET /api/v1/licenses/{key}     single entry; case-insensitive lookup
//
// Mount inside the same r.Group the other curated-metadata routes
// (meta, user/plan) live in — anon-or-PAT both fine; we don't enforce
// a scope because the data is intentionally public.
func (h *Handlers) mountLicenses(r chi.Router) {
	r.Get("/api/v1/licenses", h.licensesList)
	r.Get("/api/v1/licenses/{key}", h.licenseGet)
}

type licenseEnvelope struct {
	// Key is the lowercase identifier — gh-canonical surface.
	Key string `json:"key"`
	// SPDXID is the canonical SPDX casing (e.g., "MIT", "AGPL-3.0").
	// Older shithub clients keyed off this; new clients should prefer
	// Key for case-insensitive comparison.
	SPDXID string `json:"spdx_id"`
	// Name is the human-readable SPDX title (e.g., "MIT License").
	Name string `json:"name"`
	// NodeID is the opaque identifier shape (I7b precursor). Empty
	// for now — populated once the node-id helper lands.
	NodeID string `json:"node_id,omitempty"`
}

func (h *Handlers) licensesList(w http.ResponseWriter, _ *http.Request) {
	keys := templates.Licenses()
	out := make([]licenseEnvelope, 0, len(keys))
	for _, k := range keys {
		out = append(out, licenseEnvelope{
			Key:    strings.ToLower(k),
			SPDXID: k,
			Name:   templates.LicenseName(k),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) licenseGet(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	canonical, ok := templates.LicenseCanonical(key)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "license not found")
		return
	}
	writeJSON(w, http.StatusOK, licenseEnvelope{
		Key:    strings.ToLower(canonical),
		SPDXID: canonical,
		Name:   templates.LicenseName(canonical),
	})
}
