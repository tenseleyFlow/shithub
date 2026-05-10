// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// tryRedirect resolves a (stale_owner, stale_name) URL to its current
// canonical form via repo_redirects. Returns "" when no redirect row
// matches, in which case the caller should 404. The returned URL
// preserves the request's RemainingPath so e.g. /old/repo/blob/x
// becomes /new/repo/blob/x — handy when more granular routes ship.
func (h *Handlers) tryRedirect(r *http.Request, ownerName, repoName string) string {
	owner, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, ownerName)
	if err != nil {
		return ""
	}
	repoID, err := h.rq.LookupRedirectByUserOwner(r.Context(), h.d.Pool, reposdb.LookupRedirectByUserOwnerParams{
		OldOwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		OldName:        repoName,
	})
	if err != nil {
		return ""
	}
	row, err := h.rq.GetRepoByID(r.Context(), h.d.Pool, repoID)
	if err != nil || row.DeletedAt.Valid {
		// Repo gone for real; let the caller serve a clean 404 instead
		// of redirecting into a 404. This keeps user agents from
		// chasing dead links via a 301.
		return ""
	}
	currentOwner := ""
	if row.OwnerUserID.Valid {
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, row.OwnerUserID.Int64); err == nil {
			currentOwner = u.Username
		}
	}
	if currentOwner == "" || row.Name == "" {
		return ""
	}
	// Preserve any path tail beyond /{owner}/{repo} (rare today; will
	// matter when blob/tree/issues subpaths land).
	tail := strings.TrimPrefix(r.URL.Path, "/"+ownerName+"/"+repoName)
	return "/" + currentOwner + "/" + row.Name + tail
}
