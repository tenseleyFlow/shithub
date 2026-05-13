// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

type Feature string

// Feature constants. PRO02 Q5 ratified an un-namespaced naming
// convention with per-feature AppliesTo() metadata — features
// shared between user and org repos drop the `Org` infix; features
// scoped to org-owned resources (actions secrets / variables) keep
// it because the *resource* is org-scoped, not because the *gate*
// is. The kind-applicability registry lives in featureKinds below.
const (
	FeatureSecretTeams              Feature = "secret_teams"
	FeatureAdvancedBranchProtection Feature = "advanced_branch_protection"
	FeatureRequiredReviewers        Feature = "required_reviewers"
	FeatureActionsOrgSecrets        Feature = "actions_org_secrets" // #nosec G101 -- entitlement feature key, not a credential.
	FeatureActionsOrgVariables      Feature = "actions_org_variables"
	FeaturePrivateCollaboration     Feature = "private_collaboration_limit"
	FeatureStorageQuota             Feature = "storage_quota"
	FeatureActionsMinutesQuota      Feature = "actions_minutes_quota"
)

// Deprecated aliases. Old call sites continue to compile; PRO05's
// sweep migrates them. Remove after PRO07 enforce flip stabilizes.
const (
	FeatureOrgSecretTeams              = FeatureSecretTeams
	FeatureOrgAdvancedBranchProtection = FeatureAdvancedBranchProtection
	FeatureOrgRequiredReviewers        = FeatureRequiredReviewers
	FeatureOrgActionsSecrets           = FeatureActionsOrgSecrets
	FeatureOrgActionsVariables         = FeatureActionsOrgVariables
	FeatureOrgPrivateCollaboration     = FeaturePrivateCollaboration
	FeatureOrgStorageQuota             = FeatureStorageQuota
	FeatureOrgActionsMinutesQuota      = FeatureActionsMinutesQuota
)

type Limit string

const (
	FreePrivateCollaborationLimit int64 = 3

	LimitPrivateCollaboration Limit = "private_collaboration_limit"
	LimitStorageQuota         Limit = "storage_quota"
	LimitActionsMinutesQuota  Limit = "actions_minutes_quota"
)

// Deprecated limit aliases. Same migration story as features.
const (
	LimitOrgPrivateCollaboration = LimitPrivateCollaboration
	LimitOrgStorageQuota         = LimitStorageQuota
	LimitOrgActionsMinutesQuota  = LimitActionsMinutesQuota
)

// featureKinds is the AppliesTo registry. Each feature lists the
// principal kinds it applies to. requirePrincipalFeature rejects
// calls with a kind the feature doesn't apply to — surfacing the
// mistake at handler boot time, not at request time.
//
// PRO02 Q5 ratified Option A (un-namespaced constants with this
// registry) and PRO01 ratified the per-feature applicability.
// FeaturePrivateCollaboration stays org-only — PRO01 explicitly
// rejected reintroducing a Free user collaborator cap.
var featureKinds = map[Feature][]billing.SubjectKind{
	FeatureSecretTeams:              {billing.SubjectKindOrg},
	FeatureAdvancedBranchProtection: {billing.SubjectKindUser, billing.SubjectKindOrg},
	FeatureRequiredReviewers:        {billing.SubjectKindUser, billing.SubjectKindOrg},
	FeatureActionsOrgSecrets:        {billing.SubjectKindOrg},
	FeatureActionsOrgVariables:      {billing.SubjectKindOrg},
	FeaturePrivateCollaboration:     {billing.SubjectKindOrg},
	FeatureStorageQuota:             {billing.SubjectKindOrg}, // user pending SP08
	FeatureActionsMinutesQuota:      {billing.SubjectKindOrg}, // user pending SP08
}

// AppliesTo reports the principal kinds a feature applies to.
// Returns nil for unknown features.
func AppliesTo(feature Feature) []billing.SubjectKind {
	return featureKinds[feature]
}

// FeatureAppliesToKind is the handler-side guard: returns true when
// `feature` is valid for `kind`. Use this to short-circuit gating
// when the principal kind doesn't even apply to the feature (org-
// only features are inapplicable on personal repos and should
// fall through to the basic, ungated behavior).
func FeatureAppliesToKind(feature Feature, kind billing.SubjectKind) bool {
	for _, k := range featureKinds[feature] {
		if k == kind {
			return true
		}
	}
	return false
}

type Reason string

const (
	ReasonNone                   Reason = ""
	ReasonUpgradeRequired        Reason = "upgrade_required"
	ReasonBillingActionNeeded    Reason = "billing_action_needed"
	ReasonEnterpriseContactSales Reason = "enterprise_contact_sales"
	ReasonUnknownFeature         Reason = "unknown_feature"
)

type Deps struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

type Decision struct {
	Feature      Feature
	Allowed      bool
	RequiredPlan billing.Plan
	Reason       Reason
}

type LimitValue struct {
	Name         Limit
	Feature      Feature
	Allowed      bool
	Defined      bool
	Unlimited    bool
	Value        int64
	Unit         string
	RequiredPlan billing.Plan
	Reason       Reason
}

type UpgradeBanner struct {
	Message    string
	ActionText string
	ActionHref string
	StatusCode int
}

type Loader struct {
	deps Deps
}

// Set carries the resolved entitlement state for a principal.
// Post-PRO05 the canonical routing key is `Principal`; OrgID and
// State are kept populated for org-kind sets so SP-era callers
// continue to read them without modification. User-kind sets
// populate `UserState` and leave `State` zero.
type Set struct {
	OrgID     int64             // populated for org kind only (deprecated; prefer Principal.ID)
	Principal billing.Principal // PRO05: canonical routing key
	State     billing.State     // org-kind billing state (zero for user kind)
	UserState billing.UserState // user-kind billing state (zero for org kind)
	now       time.Time
}

var (
	ErrPoolRequired   = errors.New("entitlements: pool is required")
	ErrOrgIDRequired  = errors.New("entitlements: org id is required")
	ErrUnknownFeature = errors.New("entitlements: unknown feature")
	ErrUnknownLimit   = errors.New("entitlements: unknown limit")
)

func New(deps Deps) Loader {
	return Loader{deps: deps}
}

func ForOrg(ctx context.Context, deps Deps, orgID int64) (Set, error) {
	return New(deps).ForOrg(ctx, orgID)
}

func (l Loader) ForOrg(ctx context.Context, orgID int64) (Set, error) {
	return l.ForPrincipal(ctx, billing.PrincipalForOrg(orgID))
}

// ForPrincipal is the kind-agnostic loader. Branches to the org or
// user billing-state table based on `p.Kind` and returns a Set
// whose CanUse / Limit decisions reflect the principal's
// subscription state. PRO05+ callers prefer this entry point over
// ForOrg.
func ForPrincipal(ctx context.Context, deps Deps, p billing.Principal) (Set, error) {
	return New(deps).ForPrincipal(ctx, p)
}

func (l Loader) ForPrincipal(ctx context.Context, p billing.Principal) (Set, error) {
	if l.deps.Pool == nil {
		return Set{}, ErrPoolRequired
	}
	if err := p.Validate(); err != nil {
		return Set{}, err
	}
	now := time.Now().UTC()
	if l.deps.Now != nil {
		now = l.deps.Now().UTC()
	}
	bd := billing.Deps{Pool: l.deps.Pool}
	switch p.Kind {
	case billing.SubjectKindOrg:
		state, err := billing.GetOrgBillingState(ctx, bd, p.ID)
		if err != nil {
			return Set{}, err
		}
		return Set{OrgID: p.ID, Principal: p, State: state, now: now}, nil
	case billing.SubjectKindUser:
		state, err := billing.GetUserBillingState(ctx, bd, p.ID)
		if err != nil {
			return Set{}, err
		}
		return Set{Principal: p, UserState: state, now: now}, nil
	default:
		return Set{}, billing.ErrInvalidPrincipal
	}
}

func CheckOrgFeature(ctx context.Context, deps Deps, orgID int64, feature Feature) (Decision, error) {
	return CheckPrincipalFeature(ctx, deps, billing.PrincipalForOrg(orgID), feature)
}

// CheckPrincipalFeature is the kind-agnostic decision shortcut.
// Loads the principal's state and returns the Decision for
// `feature`. The unknown-feature check covers both renamed and
// deprecated-alias forms.
func CheckPrincipalFeature(ctx context.Context, deps Deps, p billing.Principal, feature Feature) (Decision, error) {
	if !KnownFeature(feature) {
		return Decision{}, ErrUnknownFeature
	}
	set, err := ForPrincipal(ctx, deps, p)
	if err != nil {
		return Decision{}, err
	}
	return set.CanUse(feature), nil
}

// CanUse evaluates `feature` against the carried Principal's
// current state. For org kind, behavior is unchanged from SP05.
// For user kind (PRO05+), feature must be in the user AppliesTo
// list and the user-state Plan/Status branch decides.
func (s Set) CanUse(feature Feature) Decision {
	if s.Principal.Kind == billing.SubjectKindUser {
		return decideUserFeature(s.now, s.UserState, feature)
	}
	// Default / org kind: existing flow.
	return decideOrgFeature(s.now, s.State, feature)
}

func (s Set) Limit(name Limit) (LimitValue, error) {
	feature, unit, ok := limitFeature(name)
	if !ok {
		return LimitValue{
			Name:   name,
			Reason: ReasonUnknownFeature,
		}, ErrUnknownLimit
	}
	decision := s.CanUse(feature)
	value := LimitValue{
		Name:         name,
		Feature:      feature,
		Allowed:      decision.Allowed,
		Unit:         unit,
		RequiredPlan: decision.RequiredPlan,
		Reason:       decision.Reason,
	}
	if !decision.Allowed {
		return value, nil
	}
	switch name {
	case LimitOrgPrivateCollaboration:
		value.Defined = true
		if decision.Allowed {
			value.Unlimited = true
		} else {
			value.Value = FreePrivateCollaborationLimit
		}
	case LimitOrgStorageQuota, LimitOrgActionsMinutesQuota:
		// SP08 owns usage accounting and concrete quota numbers. Until
		// then, expose entitlement state without pretending metering is enforced.
		value.Defined = false
	}
	return value, nil
}

// KnownFeature reports whether `feature` is in the registry. Used
// by handler-side validation; PRO05 onwards prefers
// FeatureAppliesToKind for kind-aware checks.
func KnownFeature(feature Feature) bool {
	_, ok := featureKinds[feature]
	return ok
}

func KnownLimit(name Limit) bool {
	_, _, ok := limitFeature(name)
	return ok
}

func (d Decision) HTTPStatus() int {
	if d.Allowed {
		return http.StatusOK
	}
	return http.StatusPaymentRequired
}

// BillingPath returns the settings page URL for the orgSlug. For
// PRO06+ user-tier upgrades, use UserBillingPath instead.
func (d Decision) BillingPath(orgSlug string) string {
	return "/organizations/" + url.PathEscape(orgSlug) + "/settings/billing"
}

// UserBillingPath returns the user's settings page URL. PRO06 wires
// `/settings/billing` for personal accounts; this helper hides the
// route from callers that just need a target.
func (d Decision) UserBillingPath() string {
	return "/settings/billing"
}

// UpgradeBanner returns the org-flavored banner. Kept for SP-era
// callers; PRO05+ callers should prefer PrincipalUpgradeBanner
// which selects copy + path based on the Decision's required plan.
func (d Decision) UpgradeBanner(label, orgSlug string) UpgradeBanner {
	banner := UpgradeBanner{
		ActionText: "Manage billing and plans",
		ActionHref: d.BillingPath(orgSlug),
		StatusCode: d.HTTPStatus(),
	}
	switch d.Reason {
	case ReasonBillingActionNeeded:
		banner.Message = label + " are read-only until Team billing is brought back into good standing."
	case ReasonEnterpriseContactSales:
		banner.Message = label + " require a supported enterprise plan. Contact sales to continue."
	default:
		banner.Message = label + " require Team billing. Upgrade this organization to continue."
	}
	return banner
}

// PrincipalUpgradeBanner returns the banner appropriate to the
// principal kind. User-kind banners say "Upgrade to Pro" and point
// at /settings/billing; org-kind banners say "Upgrade to Team" and
// point at the org billing settings page.
func (d Decision) PrincipalUpgradeBanner(label string, p billing.Principal, orgSlug string) UpgradeBanner {
	if p.IsUser() {
		banner := UpgradeBanner{
			ActionText: "Manage billing",
			ActionHref: d.UserBillingPath(),
			StatusCode: d.HTTPStatus(),
		}
		switch d.Reason {
		case ReasonBillingActionNeeded:
			banner.Message = label + " are read-only until Pro billing is brought back into good standing."
		default:
			banner.Message = label + " require Pro billing. Upgrade this account to continue."
		}
		return banner
	}
	return d.UpgradeBanner(label, orgSlug)
}

// requiredPlanForFeature returns the plan that unlocks `feature`
// for `kind`. Returns "" for unknown features or kinds the feature
// doesn't apply to. PRO04+PRO05 ratification: user kind ⇒ PlanPro;
// org kind ⇒ PlanTeam. No third option exists in PRO04's scope;
// enterprise is contact-sales (not self-serve unlock).
func requiredPlanForFeature(feature Feature, kind billing.SubjectKind) billing.Plan {
	if !FeatureAppliesToKind(feature, kind) {
		return ""
	}
	switch kind {
	case billing.SubjectKindOrg:
		return billing.PlanTeam
	case billing.SubjectKindUser:
		// Note: PlanPro is a user_plan enum value; billing.PlanFree
		// etc. are org_plan. The cross-table-ness is deliberate per
		// PRO02 Q2 (separate enums). The Decision type holds the
		// returned plan as the org-plan-typed billing.Plan because
		// pre-PRO05 callers assume that. PRO05 keeps the field
		// shape and writes "pro" in there as a string — Plan is a
		// type alias for billingdb.OrgPlan which is just a string
		// under the hood, so this is safe and PRO06+PRO07 callers
		// that compare against PlanTeam continue to work.
		return billing.Plan("pro")
	}
	return ""
}

func limitFeature(name Limit) (Feature, string, bool) {
	switch name {
	case LimitPrivateCollaboration:
		return FeaturePrivateCollaboration, "collaborators", true
	case LimitStorageQuota:
		return FeatureStorageQuota, "bytes", true
	case LimitActionsMinutesQuota:
		return FeatureActionsMinutesQuota, "minutes", true
	default:
		return "", "", false
	}
}

// decideUserFeature is the user-kind decision body. Mirrors
// decideOrgFeature's structure with user_plan ('free' | 'pro') in
// place of org_plan and `pro` as the unlocking plan. PRO07 flips
// enforcement; PRO05 (and PRO06) keep this path live for the
// CanUse return value but handler-side gating runs in report-only
// mode so the deny is logged not surfaced.
func decideUserFeature(now time.Time, state billing.UserState, feature Feature) Decision {
	decision := Decision{
		Feature:      feature,
		RequiredPlan: requiredPlanForFeature(feature, billing.SubjectKindUser),
		Reason:       ReasonUpgradeRequired,
	}
	if decision.RequiredPlan == "" {
		decision.Reason = ReasonUnknownFeature
		return decision
	}
	switch state.Plan {
	case billing.UserPlanPro:
		switch state.SubscriptionStatus {
		case billing.SubscriptionStatusActive,
			billing.SubscriptionStatusTrialing:
			decision.Allowed = true
			decision.Reason = ReasonNone
			return decision
		case billing.SubscriptionStatusPastDue:
			if state.GraceUntil.Valid && !now.After(state.GraceUntil.Time) {
				decision.Allowed = true
				decision.Reason = ReasonNone
				return decision
			}
			decision.Reason = ReasonBillingActionNeeded
			return decision
		default:
			decision.Reason = ReasonBillingActionNeeded
			return decision
		}
	default:
		// Free user — feature gated.
		return decision
	}
}

// decideOrgFeature is the org-kind decision body. Kept as the
// existing flow so org callers see byte-for-byte identical
// behavior; the user-kind path lives in decideUserFeature.
func decideOrgFeature(now time.Time, state billing.State, feature Feature) Decision {
	decision := Decision{
		Feature:      feature,
		RequiredPlan: requiredPlanForFeature(feature, billing.SubjectKindOrg),
		Reason:       ReasonUpgradeRequired,
	}
	if decision.RequiredPlan == "" {
		decision.Reason = ReasonUnknownFeature
		return decision
	}
	switch state.Plan {
	case billing.PlanEnterprise:
		decision.Reason = ReasonEnterpriseContactSales
		return decision
	case billing.PlanTeam:
		switch state.SubscriptionStatus {
		case billing.SubscriptionStatusActive,
			billing.SubscriptionStatusTrialing:
			decision.Allowed = true
			decision.Reason = ReasonNone
			return decision
		case billing.SubscriptionStatusPastDue:
			if state.GraceUntil.Valid && !now.After(state.GraceUntil.Time) {
				decision.Allowed = true
				decision.Reason = ReasonNone
				return decision
			}
			decision.Reason = ReasonBillingActionNeeded
			return decision
		default:
			decision.Reason = ReasonBillingActionNeeded
			return decision
		}
	default:
		return decision
	}
}
