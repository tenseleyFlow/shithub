// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	repoinsights "github.com/tenseleyFlow/shithub/internal/repos/insights"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const (
	repoInsightsActivePulse          = "pulse"
	repoInsightsActiveContributors   = "contributors"
	repoInsightsActiveCommitActivity = "commit-activity"
	repoInsightsActiveCodeFrequency  = "code-frequency"
	repoInsightsActiveTraffic        = "traffic"
	repoInsightsActiveNetwork        = "network"
)

type repoInsightsView struct {
	Active        string
	Heading       string
	Description   string
	Nav           []repoInsightsNavItem
	Snapshot      *repoinsights.Snapshot
	NeedsSnapshot bool
	Queued        bool
	Stale         bool
	Network       repoInsightsNetwork
	Traffic       repotraffic.Summary
}

type repoInsightsNavItem struct {
	Key    string
	Label  string
	Href   string
	Icon   string
	Active bool
}

type repoInsightsNetwork struct {
	Forks []repoInsightsFork
	Total int
}

type repoInsightsFork struct {
	OwnerUsername string
	Name          string
	Description   string
	Visibility    string
	StarCount     int64
	ForkCount     int64
}

func (h *Handlers) repoInsightsPulse(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActivePulse)
}

func (h *Handlers) repoInsightsContributors(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActiveContributors)
}

func (h *Handlers) repoInsightsCommitActivity(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActiveCommitActivity)
}

func (h *Handlers) repoInsightsCodeFrequency(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActiveCodeFrequency)
}

func (h *Handlers) repoInsightsTraffic(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActiveTraffic)
}

func (h *Handlers) repoInsightsNetwork(w http.ResponseWriter, r *http.Request) {
	h.renderRepoInsights(w, r, repoInsightsActiveNetwork)
}

func (h *Handlers) renderRepoInsights(w http.ResponseWriter, r *http.Request, active string) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	if !h.requireRepoFeature(w, r, row, owner.Username, entitlements.FeatureRepoInsights, "Repository insights") {
		return
	}

	view, err := h.repoInsightsView(r.Context(), row, owner.Username, active)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo insights", "repo_id", row.ID, "active", active, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "load insights")
		return
	}
	data := h.repoHeaderData(r, row, owner.Username, "insights")
	data["Title"] = view.Heading + " · " + row.Name
	data["Insights"] = view
	if err := h.d.Render.RenderPage(w, r, "repo/insights", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo insights render", "repo_id", row.ID, "active", active, "error", err)
	}
}

func (h *Handlers) repoInsightsView(ctx context.Context, row reposdb.Repo, owner string, active string) (repoInsightsView, error) {
	view := repoInsightsView{
		Active:        active,
		Heading:       repoInsightsHeading(active),
		Description:   repoInsightsDescription(active),
		Nav:           repoInsightsNav(owner, row.Name, active),
		NeedsSnapshot: active != repoInsightsActiveTraffic && active != repoInsightsActiveNetwork,
	}
	if view.NeedsSnapshot {
		snapshot, queued, stale, err := h.loadRepoInsightsSnapshot(ctx, row)
		if err != nil {
			return view, err
		}
		view.Snapshot = snapshot
		view.Queued = queued
		view.Stale = stale
	}
	if active == repoInsightsActiveTraffic {
		traffic, err := repotraffic.LoadSummary(ctx, h.d.Pool, row.ID, time.Now())
		if err != nil {
			return view, err
		}
		view.Traffic = traffic
	}
	if active == repoInsightsActiveNetwork {
		network, err := h.repoInsightsNetworkData(ctx, row)
		if err != nil {
			return view, err
		}
		view.Network = network
	}
	return view, nil
}

func (h *Handlers) loadRepoInsightsSnapshot(ctx context.Context, row reposdb.Repo) (*repoinsights.Snapshot, bool, bool, error) {
	dbrow, err := h.rq.GetRepoInsightSnapshot(ctx, h.d.Pool, row.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.enqueueRepoInsightsRefresh(ctx, row.ID)
			return nil, true, false, nil
		}
		return nil, false, false, fmt.Errorf("get snapshot: %w", err)
	}
	var snapshot repoinsights.Snapshot
	if err := json.Unmarshal(dbrow.Data, &snapshot); err != nil {
		return nil, false, false, fmt.Errorf("decode snapshot: %w", err)
	}
	stale := row.DefaultBranchOid.Valid && snapshot.HeadSHA != "" && snapshot.HeadSHA != row.DefaultBranchOid.String
	if stale {
		h.enqueueRepoInsightsRefresh(ctx, row.ID)
	}
	return &snapshot, false, stale, nil
}

func (h *Handlers) enqueueRepoInsightsRefresh(ctx context.Context, repoID int64) {
	if _, err := worker.Enqueue(ctx, h.d.Pool, worker.KindRepoInsightsRecalc,
		map[string]any{"repo_id": repoID}, worker.EnqueueOptions{}); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "repo insights: enqueue refresh", "repo_id", repoID, "error", err)
	}
}

func (h *Handlers) repoInsightsNetworkData(ctx context.Context, row reposdb.Repo) (repoInsightsNetwork, error) {
	const maxForks = 50
	forks, err := h.rq.ListForksOfRepo(ctx, h.d.Pool, reposdb.ListForksOfRepoParams{
		ForkOfRepoID: pgtype.Int8{Int64: row.ID, Valid: true},
		Limit:        maxForks,
		Offset:       0,
	})
	if err != nil {
		return repoInsightsNetwork{}, err
	}
	total, err := h.rq.CountForksOfRepo(ctx, h.d.Pool, pgtype.Int8{Int64: row.ID, Valid: true})
	if err != nil {
		return repoInsightsNetwork{}, err
	}
	viewer := middleware.CurrentUserFromContext(ctx)
	actor := actorFor(viewer)
	deps := policy.Deps{Pool: h.d.Pool}
	out := repoInsightsNetwork{Total: int(total), Forks: make([]repoInsightsFork, 0, len(forks))}
	for _, fk := range forks {
		ref := policy.RepoRef{ID: fk.ID, Visibility: string(fk.Visibility)}
		if !policy.IsVisibleTo(ctx, deps, actor, ref) {
			continue
		}
		out.Forks = append(out.Forks, repoInsightsFork{
			OwnerUsername: fk.OwnerUsername,
			Name:          fk.Name,
			Description:   fk.Description,
			Visibility:    string(fk.Visibility),
			StarCount:     fk.StarCount,
			ForkCount:     fk.ForkCount,
		})
	}
	return out, nil
}

func repoInsightsHeading(active string) string {
	switch active {
	case repoInsightsActiveContributors:
		return "Contributors"
	case repoInsightsActiveCommitActivity:
		return "Commit activity"
	case repoInsightsActiveCodeFrequency:
		return "Code frequency"
	case repoInsightsActiveTraffic:
		return "Traffic"
	case repoInsightsActiveNetwork:
		return "Network"
	default:
		return "Pulse"
	}
}

func repoInsightsDescription(active string) string {
	switch active {
	case repoInsightsActiveContributors:
		return "Contributions by author across the repository's default branch history."
	case repoInsightsActiveCommitActivity:
		return "Weekly commit volume for the default branch."
	case repoInsightsActiveCodeFrequency:
		return "Weekly additions and deletions for the default branch."
	case repoInsightsActiveTraffic:
		return "Views and clones recorded for this repository."
	case repoInsightsActiveNetwork:
		return "Forks connected to this repository."
	default:
		return "A 30-day summary of commits, contributors, and changed files."
	}
}

func repoInsightsNav(owner, repoName, active string) []repoInsightsNavItem {
	items := []repoInsightsNavItem{
		{Key: repoInsightsActivePulse, Label: "Pulse", Icon: "pulse", Href: "/" + owner + "/" + repoName + "/pulse"},
		{Key: repoInsightsActiveContributors, Label: "Contributors", Icon: "people", Href: "/" + owner + "/" + repoName + "/graphs/contributors"},
		{Key: repoInsightsActiveCommitActivity, Label: "Commits", Icon: "git-commit", Href: "/" + owner + "/" + repoName + "/graphs/commit-activity"},
		{Key: repoInsightsActiveCodeFrequency, Label: "Code frequency", Icon: "diff", Href: "/" + owner + "/" + repoName + "/graphs/code-frequency"},
		{Key: repoInsightsActiveTraffic, Label: "Traffic", Icon: "eye", Href: "/" + owner + "/" + repoName + "/graphs/traffic"},
		{Key: repoInsightsActiveNetwork, Label: "Network", Icon: "repo-forked", Href: "/" + owner + "/" + repoName + "/network"},
		{Key: "forks", Label: "Forks", Icon: "repo-forked", Href: "/" + owner + "/" + repoName + "/forks"},
	}
	for i := range items {
		items[i].Active = items[i].Key == active
	}
	return items
}
