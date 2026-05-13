// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

// RequireOpts tunes the behavior of RequirePrincipalFeature.
//
// EnforceForUser flips whether a user-kind deny actually blocks
// the request (true) or merely logs a would-deny while letting
// the request proceed (false). PRO05 ships with EnforceForUser =
// false across all gating sites; PRO07 flips it per-feature via
// operator-controlled per-feature config flags.
//
// EnforceForOrg defaults to true — org-tier gating has been enforced
// since SP05 and should not silently flip to report-only. Operators
// who need to relax an org gate set this to false explicitly.
//
// Label is the user-visible feature name baked into the 402 banner
// message; OrgSlug is the path component for the org settings page.
type RequireOpts struct {
	Label          string
	OrgSlug        string // ignored for user kind
	EnforceForOrg  bool   // default true via NewRequireOpts
	EnforceForUser bool   // default false in PRO05; PRO07 flips per feature
	WriteResponse  bool   // default true: write 402; false: just return false for HTML callers
	Logger         *slog.Logger
}

// NewRequireOpts is the sane-default constructor: enforce on org,
// report-only on user, write 402 on deny, no logger (caller supplies).
func NewRequireOpts(label, orgSlug string) RequireOpts {
	return RequireOpts{
		Label:          label,
		OrgSlug:        orgSlug,
		EnforceForOrg:  true,
		EnforceForUser: false,
		WriteResponse:  true,
	}
}

// EnforceModeDecision packages the decision plus the chosen
// enforcement mode for the caller. Handlers usually consume `Allow`
// directly; tests and telemetry use the rest for assertions.
type EnforceModeDecision struct {
	Decision              Decision
	Principal             billing.Principal
	Allow                 bool // final answer accounting for report-only
	WouldDenyButLog       bool // user-kind report-only path took this branch
	UnknownKindForFeature bool // feature doesn't apply to principal kind — short-circuit allow
}

// RequirePrincipalFeature is the canonical handler-side gate.
// Returns ok=true when the request may proceed (either the user has
// the feature OR a report-only mode is allowing the request despite
// a would-deny). Returns ok=false only when the caller should stop;
// the helper has already written the response if WriteResponse=true.
//
// Behavior by principal kind:
//
//   - Org with EnforceForOrg=true (default): existing SP05 semantics.
//     Deny writes 402 + org-flavored banner.
//   - User with EnforceForUser=false (PRO05 default): logs would-deny
//     at info level, returns ok=true. Telemetry counters can then
//     confirm no Free user is currently exercising a Pro-gated path
//     before PRO07 enforces.
//   - Either kind, feature doesn't apply: short-circuit ok=true with
//     no log. Org-only features on personal repos are a no-op.
func RequirePrincipalFeature(
	ctx context.Context,
	w http.ResponseWriter,
	pool *pgxpool.Pool,
	p billing.Principal,
	feature Feature,
	opts RequireOpts,
) (bool, EnforceModeDecision) {
	res := EnforceModeDecision{Principal: p}

	// Feature applicability short-circuit: org-only features on a
	// user principal (or vice versa) are not enforced here — the
	// caller's intent was "if this principal owns the resource,
	// gate it on this feature", and an inapplicable feature means
	// the resource type isn't gateable for this kind. The handler
	// proceeds with its existing default behavior.
	if !FeatureAppliesToKind(feature, p.Kind) {
		res.UnknownKindForFeature = true
		res.Allow = true
		return true, res
	}

	decision, err := CheckPrincipalFeature(ctx, Deps{Pool: pool}, p, feature)
	if err != nil {
		if opts.Logger != nil {
			opts.Logger.ErrorContext(ctx, "entitlements: principal feature check failed",
				"principal", p.String(),
				"feature", feature,
				"error", err)
		}
		if opts.WriteResponse {
			http.Error(w, "entitlement check failed", http.StatusInternalServerError)
		}
		return false, res
	}
	res.Decision = decision

	if decision.Allowed {
		res.Allow = true
		return true, res
	}

	enforce := false
	switch p.Kind {
	case billing.SubjectKindOrg:
		enforce = opts.EnforceForOrg
	case billing.SubjectKindUser:
		enforce = opts.EnforceForUser
	}

	if !enforce {
		// Report-only path: log + allow.
		res.WouldDenyButLog = true
		res.Allow = true
		if opts.Logger != nil {
			opts.Logger.InfoContext(ctx, "entitlements.report_only_deny",
				"principal", p.String(),
				"principal_kind", string(p.Kind),
				"principal_id", p.ID,
				"feature", feature,
				"reason", string(decision.Reason),
				"required_plan", string(decision.RequiredPlan))
		}
		return true, res
	}

	// Enforce path: write 402 + banner, return false.
	if opts.WriteResponse {
		banner := decision.PrincipalUpgradeBanner(opts.Label, p, opts.OrgSlug)
		http.Error(w, banner.Message, banner.StatusCode)
	}
	return false, res
}

// ErrUnknownPrincipalKind is returned by callers that want to
// surface the inapplicability of a feature to a principal kind as
// a hard error rather than the short-circuit allow that
// RequirePrincipalFeature does.
var ErrUnknownPrincipalKind = errors.New("entitlements: feature does not apply to principal kind")
