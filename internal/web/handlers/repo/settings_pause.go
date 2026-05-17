// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-15: per-repo pause/unpause routes.
//
// Pause is a Pro-tier soft-freeze distinct from archive. The handler
// gates on FeatureRepoTimeMachine — Free users hit a 402 with an
// upgrade banner when enforce is on, or a report-only log when off.
//
// Authorization (admin role on the repo) is checked via
// loadRepoAndAuthorize + policy.ActionRepoArchive — we reuse the
// archive action's role mapping (RoleAdmin) rather than minting a
// separate ActionRepoPause, since the role policy is identical and
// the entitlement gate carries the Pro-vs-Free distinction. If a
// future sprint wants to grant pause to a lower role, split it then.

func (h *Handlers) repoPause(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoArchive)
	if !ok {
		return
	}
	if !h.repoPauseGate(w, r, row, "pause") {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	reason := r.PostFormValue("pause_reason")
	if err := lifecycle.Pause(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID, reason); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=paused", http.StatusSeeOther)
}

func (h *Handlers) repoUnpause(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoArchive)
	if !ok {
		return
	}
	// Unpause is unconditionally allowed for admins — we don't want
	// a Pro-→Free downgrade to leave the owner unable to unstuck
	// their repo. The pause direction is the only side gated.
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := lifecycle.Unpause(r.Context(), h.lifecycleDeps(), viewer.ID, row.ID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	policy.InvalidateRepo(r.Context(), row.ID)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/settings?notice=unpaused", http.StatusSeeOther)
}

// repoPauseGate enforces FeatureRepoTimeMachine. Returns true to
// proceed. Logs report_only_deny on a non-allowed Free attempt; only
// blocks the call when the operator's enforce flag is on. Surface
// tag for SREs: settings-pause.
func (h *Handlers) repoPauseGate(w http.ResponseWriter, r *http.Request, row reposdb.Repo, action string) bool {
	principal, ok := principalFromRepo(row)
	if !ok {
		// No owner principal → no entitlement to check. Treat as
		// allowed (the lifecycle layer enforces ownership separately).
		return true
	}
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool}, principal, entitlements.FeatureRepoTimeMachine)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo pause: entitlement check", "error", err)
		http.Error(w, "entitlement check failed", http.StatusInternalServerError)
		return false
	}
	if !decision.Allowed {
		mode := "report_only"
		if h.d.BillingEnforce.UserRepoTimeMachine {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", principal.ID,
			"feature", string(entitlements.FeatureRepoTimeMachine),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "settings-pause",
			"action", action)
		if h.d.BillingEnforce.UserRepoTimeMachine {
			http.Error(w,
				"pausing repositories requires a Pro subscription — see /settings/billing",
				http.StatusPaymentRequired)
			return false
		}
	}
	return true
}
