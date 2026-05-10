// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type repoDeferredTab struct {
	Active      string
	Heading     string
	Description string
	Icon        string
	Sections    []repoDeferredSection
}

type repoDeferredSection struct {
	Anchor string
	Title  string
	Body   string
}

func (h *Handlers) repoTabActions(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "actions",
		Heading:     "Actions",
		Description: "Automate your development workflow with repository workflows and check runs.",
		Icon:        "play",
		Sections: []repoDeferredSection{
			{Anchor: "all-workflows", Title: "All workflows", Body: "Workflow execution is parked for the Actions sprint. Check runs posted by external systems still appear on pull requests."},
			{Anchor: "workflows", Title: "Workflows", Body: "Add workflow files later under .shithub/workflows to run jobs on push and pull request events."},
		},
	})
}

func (h *Handlers) repoTabProjects(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "projects",
		Heading:     "Projects",
		Description: "Organize issues and pull requests with repository project views.",
		Icon:        "table",
		Sections: []repoDeferredSection{
			{Anchor: "open", Title: "Open", Body: "No projects have been created for this repository."},
			{Anchor: "closed", Title: "Closed", Body: "Closed repository projects will appear here."},
		},
	})
}

func (h *Handlers) repoTabWiki(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "wiki",
		Heading:     "Wiki",
		Description: "Create long-form documentation pages connected to this repository.",
		Icon:        "book",
		Sections: []repoDeferredSection{
			{Anchor: "pages", Title: "Pages", Body: "No wiki pages have been created yet."},
		},
	})
}

func (h *Handlers) repoTabSecurity(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "security",
		Heading:     "Security and quality",
		Description: "Review repository security posture, policy files, alerts, and dependency health.",
		Icon:        "shield-check",
		Sections: []repoDeferredSection{
			{Anchor: "overview", Title: "Overview", Body: "Security overview is not wired yet."},
			{Anchor: "policy", Title: "Policy", Body: "Security policy detection already appears in the repository About sidebar when SECURITY.md exists."},
			{Anchor: "alerts", Title: "Alerts", Body: "Code scanning, secret scanning, and dependency alerts are reserved for later security sprints."},
		},
	})
}

func (h *Handlers) repoTabInsights(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "insights",
		Heading:     "Insights",
		Description: "Understand activity, contributors, traffic, community health, and dependency graph data.",
		Icon:        "pulse",
		Sections: []repoDeferredSection{
			{Anchor: "pulse", Title: "Pulse", Body: "Repository pulse data is not available yet."},
			{Anchor: "contributors", Title: "Contributors", Body: "Contributor graphs will be derived from git history in a later pass."},
			{Anchor: "community", Title: "Community", Body: "README, license, code of conduct, contributing, and security files are already detected on the Code tab."},
		},
	})
}

func (h *Handlers) repoTabPackages(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "packages",
		Heading:     "Packages",
		Description: "Publish and install packages associated with this repository.",
		Icon:        "package",
		Sections: []repoDeferredSection{
			{Anchor: "repository-packages", Title: "Repository packages", Body: "No packages have been published for this repository."},
		},
	})
}

func (h *Handlers) repoTabReleases(w http.ResponseWriter, r *http.Request) {
	h.renderDeferredRepoTab(w, r, repoDeferredTab{
		Active:      "releases",
		Heading:     "Releases",
		Description: "Package release notes, binaries, and source archives from repository tags.",
		Icon:        "tag",
		Sections: []repoDeferredSection{
			{Anchor: "latest-release", Title: "Latest release", Body: "No releases have been published yet."},
			{Anchor: "tags", Title: "Tags", Body: "Tags are available from the Code tab and the repository tags page."},
		},
	})
}

func (h *Handlers) renderDeferredRepoTab(w http.ResponseWriter, r *http.Request, tab repoDeferredTab) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	data := h.repoHeaderData(r, row, owner.Username, tab.Active)
	data["Title"] = tab.Heading + " · " + row.Name
	data["Tab"] = tab
	if err := h.d.Render.RenderPage(w, r, "repo/deferred_tab", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo deferred tab render", "tab", tab.Active, "error", err)
	}
}

func (h *Handlers) repoHeaderData(r *http.Request, row reposdb.Repo, owner, active string) map[string]any {
	return map[string]any{
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"Owner":        owner,
		"Repo":         row,
		"RepoActions":  h.repoActions(r, row.ID),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": active,
	}
}
