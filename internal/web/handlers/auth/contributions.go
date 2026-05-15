// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-09 — settings → contributions page.
//
// Free users can use the existing gh-parity "show private
// contributions" master toggle (lives on users.include_private_contributions;
// edited via the account settings page already). What 09 adds is the
// Pro-only **per-repo opt-out**: a checklist of the user's own repos
// where toggling the row off excludes that repo's commits from the
// public contribution heatmap.
//
// Gate posture mirrors saved replies + saved search queries: Free
// users reach the page (locked-UI affordance), the form is greyed,
// and a crafted POST in enforce mode is refused with the upgrade
// banner. Report-only logs the would-deny and lets the toggle land.

func (h *Handlers) settingsContributionsForm(w http.ResponseWriter, r *http.Request) {
	h.renderContributionsForm(w, r, "", "")
}

// settingsContributionsSubmit accepts the form-submitted list of
// opt-out repo IDs and reconciles the user's table state: any IDs in
// the form go in; any IDs in the DB but not the form are deleted.
// This makes the form idempotent from the user's perspective — they
// just submit the current state.
func (h *Handlers) settingsContributionsSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	// Pro gate. Same shape as saved replies / search queries.
	allowed, decision, err := h.contributionPrivacyAllowed(r.Context(), user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/contributions: entitlement check", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed && h.d.BillingEnforce.UserContributionPrivacy {
		banner := decision.PrincipalUpgradeBanner("Per-repo contribution privacy", billing.PrincipalForUser(user.ID), "")
		h.renderContributionsForm(w, r, banner.Message, "")
		return
	}

	// Parse the submitted opt-out repo IDs.
	desired := make(map[int64]struct{})
	for _, raw := range r.PostForm["optout_repo_id"] {
		id, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if perr != nil || id <= 0 {
			continue
		}
		desired[id] = struct{}{}
	}

	// Validate: every submitted repo ID must be one of the user's
	// own repos (defense-in-depth — a crafted form can't smuggle in a
	// repo the user doesn't own to "opt out" of someone else's heatmap).
	ownRepos := h.userOwnedRepoIDSet(r.Context(), user.ID)
	for id := range desired {
		if _, ok := ownRepos[id]; !ok {
			h.renderContributionsForm(w, r, "Selected repository is not owned by your account.", "")
			return
		}
	}

	// Reconcile: read current opt-outs; insert missing; delete extras.
	current, err := h.q.ListContributionOptoutRepoIDsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/contributions: list opt-outs", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	currentSet := make(map[int64]struct{}, len(current))
	for _, id := range current {
		currentSet[id] = struct{}{}
	}
	for id := range desired {
		if _, ok := currentSet[id]; ok {
			continue
		}
		if err := h.q.UpsertContributionOptout(r.Context(), h.d.Pool, usersdb.UpsertContributionOptoutParams{
			UserID: user.ID, RepoID: id,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "settings/contributions: insert", "user_id", user.ID, "repo_id", id, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	for id := range currentSet {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := h.q.DeleteContributionOptout(r.Context(), h.d.Pool, usersdb.DeleteContributionOptoutParams{
			UserID: user.ID, RepoID: id,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "settings/contributions: delete", "user_id", user.ID, "repo_id", id, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	h.renderContributionsForm(w, r, "", "Contribution privacy updated.")
}

// renderContributionsForm is the shared render path.
func (h *Handlers) renderContributionsForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	allowed, _, _ := h.contributionPrivacyAllowed(r.Context(), user.ID)
	repos := h.userOwnedReposListForContributions(r.Context(), user.ID)
	current, _ := h.q.ListContributionOptoutRepoIDsForUser(r.Context(), h.d.Pool, user.ID)
	optoutSet := make(map[int64]bool, len(current))
	for _, id := range current {
		optoutSet[id] = true
	}
	type repoRow struct {
		ID     int64
		Name   string
		Public bool
		OptOut bool
	}
	rows := make([]repoRow, 0, len(repos))
	for _, repo := range repos {
		rows = append(rows, repoRow{
			ID:     repo.ID,
			Name:   repo.Name,
			Public: policy.NewRepoRefFromRepo(repo).IsPublic(),
			OptOut: optoutSet[repo.ID],
		})
	}
	h.renderPage(w, r, "settings/contributions", map[string]any{
		"Title":          "Contribution privacy",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "contributions",
		"Allowed":        allowed,
		"FeatureKey":     string(entitlements.FeatureContributionPrivacy),
		"Repos":          rows,
		"Error":          errMsg,
		"Success":        successMsg,
	})
}

// contributionPrivacyAllowed wraps the entitlement check + logs the
// would-deny. Mirrors the other Pro gates.
func (h *Handlers) contributionPrivacyAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureContributionPrivacy)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureContributionPrivacy),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed, decision, nil
}

// userOwnedReposListForContributions returns the user's active owned repos
// (any visibility) sorted by name so the picker stays useful.
func (h *Handlers) userOwnedReposListForContributions(ctx context.Context, userID int64) []reposdb.Repo {
	rows, err := reposdb.New().ListActiveReposForOwnerUserByName(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "settings/contributions: list own repos", "user_id", userID, "error", err)
		return nil
	}
	return rows
}

// userOwnedRepoIDSet is the set version of userOwnedReposListForContributions
// — used to validate that a submitted opt-out repo_id is actually one
// the user owns (defense-in-depth).
func (h *Handlers) userOwnedRepoIDSet(ctx context.Context, userID int64) map[int64]struct{} {
	repos := h.userOwnedReposListForContributions(ctx, userID)
	out := make(map[int64]struct{}, len(repos))
	for _, r := range repos {
		out[r.ID] = struct{}{}
	}
	return out
}
