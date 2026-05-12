// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

type Feature string

const (
	FeatureOrgSecretTeams              Feature = "org.secret_teams"
	FeatureOrgAdvancedBranchProtection Feature = "org.advanced_branch_protection"
	FeatureOrgRequiredReviewers        Feature = "org.required_reviewers"
	FeatureOrgActionsSecrets           Feature = "org.actions_org_secrets"
	FeatureOrgActionsVariables         Feature = "org.actions_org_variables"
	FeatureOrgPrivateCollaboration     Feature = "org.private_collaboration_limit"
	FeatureOrgStorageQuota             Feature = "org.storage_quota"
	FeatureOrgActionsMinutesQuota      Feature = "org.actions_minutes_quota"
)

type Reason string

const (
	ReasonNone                Reason = ""
	ReasonUpgradeRequired     Reason = "upgrade_required"
	ReasonBillingActionNeeded Reason = "billing_action_needed"
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

var (
	ErrPoolRequired   = errors.New("entitlements: pool is required")
	ErrOrgIDRequired  = errors.New("entitlements: org id is required")
	ErrUnknownFeature = errors.New("entitlements: unknown feature")
)

func CheckOrgFeature(ctx context.Context, deps Deps, orgID int64, feature Feature) (Decision, error) {
	if deps.Pool == nil {
		return Decision{}, ErrPoolRequired
	}
	if orgID == 0 {
		return Decision{}, ErrOrgIDRequired
	}
	if requiredPlanForFeature(feature) == "" {
		return Decision{}, ErrUnknownFeature
	}
	state, err := billing.GetOrgBillingState(ctx, billing.Deps{Pool: deps.Pool}, orgID)
	if err != nil {
		return Decision{}, err
	}
	now := time.Now().UTC()
	if deps.Now != nil {
		now = deps.Now().UTC()
	}
	return decideFeature(now, state, feature), nil
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

func decideFeature(now time.Time, state billing.State, feature Feature) Decision {
	decision := Decision{
		Feature:      feature,
		RequiredPlan: requiredPlanForFeature(feature),
		Reason:       ReasonUpgradeRequired,
	}
	switch state.Plan {
	case billing.PlanEnterprise:
		decision.Allowed = true
		decision.Reason = ReasonNone
		return decision
	case billing.PlanTeam:
		switch state.SubscriptionStatus {
		case billing.SubscriptionStatusActive,
			billing.SubscriptionStatusTrialing,
			billing.SubscriptionStatusIncomplete:
			decision.Allowed = true
			decision.Reason = ReasonNone
			return decision
		case billing.SubscriptionStatusPastDue:
			if !state.GraceUntil.Valid || !now.After(state.GraceUntil.Time) {
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
