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

const (
	FeatureOrgSecretTeams              Feature = "org.secret_teams"
	FeatureOrgAdvancedBranchProtection Feature = "org.advanced_branch_protection"
	FeatureOrgRequiredReviewers        Feature = "org.required_reviewers"
	FeatureOrgActionsSecrets           Feature = "org.actions_org_secrets" // #nosec G101 -- entitlement feature key, not a credential.
	FeatureOrgActionsVariables         Feature = "org.actions_org_variables"
	FeatureOrgPrivateCollaboration     Feature = "org.private_collaboration_limit"
	FeatureOrgStorageQuota             Feature = "org.storage_quota"
	FeatureOrgActionsMinutesQuota      Feature = "org.actions_minutes_quota"
)

type Limit string

const (
	LimitOrgPrivateCollaboration Limit = "org.private_collaboration_limit"
	LimitOrgStorageQuota         Limit = "org.storage_quota"
	LimitOrgActionsMinutesQuota  Limit = "org.actions_minutes_quota"
)

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

type Set struct {
	OrgID int64
	State billing.State
	now   time.Time
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
	if l.deps.Pool == nil {
		return Set{}, ErrPoolRequired
	}
	if orgID == 0 {
		return Set{}, ErrOrgIDRequired
	}
	state, err := billing.GetOrgBillingState(ctx, billing.Deps{Pool: l.deps.Pool}, orgID)
	if err != nil {
		return Set{}, err
	}
	now := time.Now().UTC()
	if l.deps.Now != nil {
		now = l.deps.Now().UTC()
	}
	return Set{OrgID: orgID, State: state, now: now}, nil
}

func CheckOrgFeature(ctx context.Context, deps Deps, orgID int64, feature Feature) (Decision, error) {
	if !KnownFeature(feature) {
		return Decision{}, ErrUnknownFeature
	}
	set, err := ForOrg(ctx, deps, orgID)
	if err != nil {
		return Decision{}, err
	}
	return set.CanUse(feature), nil
}

func (s Set) CanUse(feature Feature) Decision {
	return decideFeature(s.now, s.State, feature)
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
		value.Unlimited = true
	case LimitOrgStorageQuota, LimitOrgActionsMinutesQuota:
		// SP08 owns usage accounting and concrete quota numbers. Until
		// then, expose entitlement state without pretending metering is enforced.
		value.Defined = false
	}
	return value, nil
}

func KnownFeature(feature Feature) bool {
	return requiredPlanForFeature(feature) != ""
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

func (d Decision) BillingPath(orgSlug string) string {
	return "/organizations/" + url.PathEscape(orgSlug) + "/settings/billing"
}

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

func requiredPlanForFeature(feature Feature) billing.Plan {
	switch feature {
	case FeatureOrgSecretTeams,
		FeatureOrgAdvancedBranchProtection,
		FeatureOrgRequiredReviewers,
		FeatureOrgActionsSecrets,
		FeatureOrgActionsVariables,
		FeatureOrgPrivateCollaboration,
		FeatureOrgStorageQuota,
		FeatureOrgActionsMinutesQuota:
		return billing.PlanTeam
	default:
		return ""
	}
}

func limitFeature(name Limit) (Feature, string, bool) {
	switch name {
	case LimitOrgPrivateCollaboration:
		return FeatureOrgPrivateCollaboration, "collaborators", true
	case LimitOrgStorageQuota:
		return FeatureOrgStorageQuota, "bytes", true
	case LimitOrgActionsMinutesQuota:
		return FeatureOrgActionsMinutesQuota, "minutes", true
	default:
		return "", "", false
	}
}

func decideFeature(now time.Time, state billing.State, feature Feature) Decision {
	decision := Decision{
		Feature:      feature,
		RequiredPlan: requiredPlanForFeature(feature),
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
