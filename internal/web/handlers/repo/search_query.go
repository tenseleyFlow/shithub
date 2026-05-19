// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	srch "github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) repoSearchActor(r *http.Request) policy.Actor {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		return policy.AnonymousActor()
	}
	return viewer.PolicyActor()
}

func (h *Handlers) repoSearchDeps() srch.Deps {
	return srch.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
}

func repoScopedIssueQuery(rawQuery, rawState, owner, repoName string) (srch.ParsedQuery, string) {
	parsed := srch.ParseQuery(rawQuery)
	state := strings.TrimSpace(rawState)
	if state == "" && parsed.StateFilter != "" {
		state = parsed.StateFilter
	}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "closed" {
		state = "open"
	}

	// Repo-local issue and PR lists must never escape the repository
	// currently being browsed, even when the typed query includes a
	// repo: qualifier. GitHub treats those pages as scoped search
	// surfaces; the global /search page owns cross-repo searches.
	parsed.RepoFilter = &srch.RepoFilter{Owner: owner, Name: repoName}
	if parsed.StateFilter == "" {
		parsed.StateFilter = state
	}
	return parsed, state
}

func (h *Handlers) searchRepoIssues(r *http.Request, parsed srch.ParsedQuery, kind string, limit, offset int) ([]srch.IssueResult, int64, error) {
	return srch.SearchIssues(r.Context(), h.repoSearchDeps(), h.repoSearchActor(r), parsed, kind, limit, offset)
}
