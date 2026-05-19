// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountIssuesAcrossRepos registers the user-scoped issue dashboard
// endpoint that `shithub pr status` and `shithub issue status` both
// depend on. G9b (F29): pre-fix the route didn't exist and both
// commands 404'd end-to-end.
//
//	GET /api/v1/issues[?filter=assigned|created|mentioned&state=open|closed|all]
//
// gh-compat: `filter=` defaults to `assigned` (the auth user's
// assigned issues). `state=` defaults to `open`. `mentioned` is
// explicitly unsupported until the @-mention index lands (mirrors
// the per-repo `mention` rejection from E4); 422 with a clear
// message so the CLI's "mentioned" branch surfaces a typed error
// rather than silent empty results.
func (h *Handlers) mountIssuesAcrossRepos(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/issues", h.issuesAcrossRepos)
	})
}

// issueAcrossReposItem is the per-row shape returned by GET /issues.
// Mirrors the per-repo issueResponse's surface that gh-compat CLI
// clients parse (number, title, state, user, repository envelope,
// html_url) — kept narrow so this list endpoint stays cheap.
type issueAcrossReposItem struct {
	ID          int64                    `json:"id"`
	Number      int64                    `json:"number"`
	Title       string                   `json:"title"`
	State       string                   `json:"state"`
	StateReason string                   `json:"state_reason,omitempty"`
	Locked      bool                     `json:"locked"`
	AuthorID    int64                    `json:"author_id,omitempty"`
	User        *userEnvelope            `json:"user,omitempty"`
	Repository  *searchIssueRepoEnvelope `json:"repository"`
	HTMLURL     string                   `json:"html_url,omitempty"`
	CreatedAt   string                   `json:"created_at"`
	UpdatedAt   string                   `json:"updated_at"`
	ClosedAt    string                   `json:"closed_at,omitempty"`
}

func (h *Handlers) issuesAcrossRepos(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("filter")))
	if filter == "" {
		filter = "assigned"
	}
	switch filter {
	case "assigned", "created":
		// supported
	case "mentioned":
		// G9b (F29): @-mention indexing isn't built yet (same gap the
		// per-repo `mention` filter rejects under E4). Surface the
		// limitation as a typed 422 so the CLI's `pr status` / `issue
		// status` mentioned-section warning is clear instead of "404
		// not found".
		writeAPIError(w, http.StatusUnprocessableEntity, "filter=mentioned is not yet supported")
		return
	default:
		writeAPIError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("filter must be one of assigned, created, mentioned (got %q)", filter))
		return
	}

	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	if state == "" {
		state = "open"
	}
	switch state {
	case "open", "closed", "all":
		// supported
	default:
		writeAPIError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("state must be one of open, closed, all (got %q)", state))
		return
	}

	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)

	visClause, visArgs := policy.VisibilityPredicate(auth.PolicyActor(), "r", 2)

	// $1 always holds the actor's user id (used by the filter join);
	// $2..$N are the visibility predicate's args; the final two are
	// limit + offset.
	args := []any{auth.UserID}
	args = append(args, visArgs...)
	limPos := len(args) + 1
	offPos := len(args) + 2
	args = append(args, perPage, (page-1)*perPage)

	var filterJoin, filterWhere string
	switch filter {
	case "assigned":
		filterJoin = "JOIN issue_assignees ia ON ia.issue_id = i.id"
		filterWhere = "ia.user_id = $1"
	case "created":
		filterJoin = ""
		filterWhere = "i.author_user_id = $1"
	}

	stateWhere := ""
	if state != "all" {
		stateWhere = " AND i.state::text = '" + state + "'"
	}

	queryStr := fmt.Sprintf(`
		SELECT i.id, i.repo_id, i.number, i.title, i.state::text,
		       coalesce(i.state_reason::text, ''),
		       i.locked, i.created_at, i.updated_at, i.closed_at,
		       i.author_user_id,
		       r.id, r.name, r.visibility::text,
		       coalesce(u.username, o.slug)::text AS owner_slug
		FROM issues i
		JOIN repos r ON r.id = i.repo_id
		LEFT JOIN users u ON u.id = r.owner_user_id
		LEFT JOIN orgs o  ON o.id = r.owner_org_id
		%s
		WHERE i.kind = 'issue'
		  AND %s
		  AND %s
		  %s
		ORDER BY i.updated_at DESC
		LIMIT $%d OFFSET $%d
	`, filterJoin, filterWhere, visClause, stateWhere, limPos, offPos)

	rows, err := h.d.Pool.Query(r.Context(), queryStr, args...)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user issues", "error", err, "filter", filter)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	defer rows.Close()

	out := make([]issueAcrossReposItem, 0, perPage)
	authorIDs := []int64{}
	for rows.Next() {
		var (
			it             issueAcrossReposItem
			repoID         int64
			repoName       string
			repoVisibility string
			ownerSlug      string
			closedAt       *time.Time
			createdAt      time.Time
			updatedAt      time.Time
			authorID       *int64
			stateReason    string
		)
		if err := rows.Scan(&it.ID, &repoID, &it.Number, &it.Title, &it.State,
			&stateReason, &it.Locked, &createdAt, &updatedAt, &closedAt,
			&authorID,
			&repoID, &repoName, &repoVisibility, &ownerSlug); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: scan user issue row", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "list failed")
			return
		}
		it.StateReason = stateReason
		it.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		it.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		if closedAt != nil {
			it.ClosedAt = closedAt.UTC().Format(time.RFC3339)
		}
		if authorID != nil && *authorID != 0 {
			it.AuthorID = *authorID
			authorIDs = append(authorIDs, *authorID)
		}
		fullName := ownerSlug + "/" + repoName
		var htmlURL, repoURL string
		if h.d.BaseURL != "" {
			repoURL = strings.TrimRight(h.d.BaseURL, "/") + "/" + fullName
			htmlURL = repoURL + "/issues/" + strconv.FormatInt(it.Number, 10)
		}
		it.HTMLURL = htmlURL
		it.Repository = &searchIssueRepoEnvelope{
			ID:       repoID,
			Name:     repoName,
			FullName: fullName,
			HTMLURL:  repoURL,
			Private:  repoVisibility != "public",
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: iterate user issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}

	// Resolve author envelopes in one batched lookup so callers see
	// `user.login` populated, not just the legacy `author_id`.
	users := h.resolveUserEnvelopesBatch(r.Context(), authorIDs)
	for i := range out {
		if out[i].AuthorID != 0 {
			out[i].User = users[out[i].AuthorID]
		}
	}

	writeJSON(w, http.StatusOK, out)
}
