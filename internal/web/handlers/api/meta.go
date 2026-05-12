// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/version"
)

// APICapabilities is the canonical list of feature capabilities exposed
// by this server. Each S50 batch (§1 user, §2 repos, §3 issues, …)
// appends its capability name here so the CLI can gate behavior. The
// values are short kebab-case identifiers, GitHub-flavored when an
// obvious mapping exists.
//
// Append-only is a soft contract: removing an entry is a breaking
// change for clients that read this list.
var APICapabilities = []string{
	"pat-auth",
	"check-runs",
	"stars",
	"actions-lifecycle",
	"user-emails",
	"ssh-keys",
	"repos",
	"issues",
	"labels",
	"pulls",
}

type metaResponse struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit"`
	BuiltAt      string   `json:"built_at"`
	Capabilities []string `json:"capabilities"`
}

// mountMeta registers the capability-discovery endpoint. /api/v1/meta is
// unauthenticated by design — clients use it to negotiate capabilities
// before they have credentials.
func (h *Handlers) mountMeta(r chi.Router) {
	r.Get("/api/v1/meta", h.metaGet)
}

func (h *Handlers) metaGet(w http.ResponseWriter, _ *http.Request) {
	caps := make([]string, len(APICapabilities))
	copy(caps, APICapabilities)
	writeJSON(w, http.StatusOK, metaResponse{
		Version:      version.Version,
		Commit:       version.Commit,
		BuiltAt:      version.BuiltAt,
		Capabilities: caps,
	})
}
