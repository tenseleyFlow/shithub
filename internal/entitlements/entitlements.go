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
	// PRO07 additions.
	//
	// FeatureProfilePinsBeyondFree gates raising the personal profile
	// pin cap above the Free baseline. Free users get LimitProfilePinsFreeCap
	// pins; Pro users get LimitProfilePinsProCap. PRO01 ratified this as
	// a shithub-specific differentiator (gh does not gate it). Kinds:
	// user only — orgs continue to share the visible Free cap.
	FeatureProfilePinsBeyondFree Feature = "profile_pins_beyond_free"
	// FeatureCodeOwnersReview is the placeholder hook for CODEOWNERS
	// enforcement. Registered now so the gating call site can compile
	// and the per-feature config knob exists; the underlying CODEOWNERS
	// parser ships in a later sprint and only then does Allowed flip.
	// PRO07 keeps the enforce path a no-op for both kinds. Kinds: user, org.
	FeatureCodeOwnersReview Feature = "codeowners_review"
	// FeatureProfileVanity gates Pro-tier profile customization: custom
	// accent color + Pro-curated pin layout. PRO-EXT01-04 ratified this
	// as a single umbrella feature rather than per-control sub-features
	// because the settings UI groups them and product copy reads cleanly
	// as one "make your profile yours" affordance. Kinds: user only.
	FeatureProfileVanity Feature = "profile_vanity"
	// FeatureAnimatedAvatars gates Pro-tier preservation of animated GIF
	// avatars. Free uploads of multi-frame GIFs are flattened to a
	// static PNG (current behavior); Pro uploads keep the raw GIF bytes
	// and the public avatar endpoint serves them with image/gif content
	// type. PRO-EXT01-04b. The decision intentionally sits OUTSIDE
	// FeatureProfileVanity so the soak window for animated serving can
	// diverge — animated GIFs are the only Pro-only avatar surface that
	// changes the *bytes* we serve, so operators may want a longer
	// observation window before flipping enforce.
	// Kinds: user only.
	FeatureAnimatedAvatars Feature = "animated_avatars"
	// FeatureUsernameReservations gates Pro-tier vanity reservations: a
	// Pro user can reserve up to ProUsernameReservationsCap inactive
	// handles against squatting. shithub-original (gh does not offer
	// this). PRO-EXT01-05. Kinds: user only.
	FeatureUsernameReservations Feature = "username_reservations"
	// FeaturePrivateRepoTemplates gates whether a user-owned *private*
	// repo can be marked as a template (and the resulting create-from-
	// template flow). Public repos remain free to mark — this matches
	// gh's posture and tightens our Pro value proposition. PRO-EXT01-06.
	// Kinds: user only (orgs have their own template policy via
	// existing org-tier gates). The gate consults the *current* plan
	// of the template owner: if the owner had Pro when they flipped
	// the flag and later lapsed, Free consumers stop being able to
	// initialize from the template.
	FeaturePrivateRepoTemplates Feature = "private_repo_templates"
	// FeatureSavedRepliesUnlimited lifts the Free saved-replies cap.
	// Free users can keep FreeSavedRepliesCap (3) entries; Pro users
	// get unlimited (DB sanity cap only). PRO-EXT01-07a. Kinds: user
	// only. The Free cap is the visible-locked surface — gh ships
	// unlimited for everyone, but shithub's Pro pitch puts a small
	// productivity-feature pack behind the gate.
	FeatureSavedRepliesUnlimited Feature = "saved_replies_unlimited"
	// FeatureScheduledIssues lets a Pro user prepare an issue and pick
	// a future datetime; a worker materializes the real issue at that
	// time. shithub-original. PRO-EXT01-07b. Kinds: user only. The
	// Free user sees the schedule control rendered as a disabled
	// pro-lock; clicking the lock CTA routes to /settings/billing.
	FeatureScheduledIssues Feature = "scheduled_issues"
	// FeatureAdvancedCodeSearch gates Pro-only code-search extensions
	// layered on top of the existing visibility-aware global search.
	// PRO-EXT01-08a ships the saved-queries CRUD; PRO-EXT01-08b adds
	// the regex query path. Kinds: user only. NOTE: this gate does
	// NOT restrict the existing /search code surface — Free users
	// keep cross-repo plain-text search; the gate only fences the new
	// Pro features (named saved queries + regex search).
	FeatureAdvancedCodeSearch Feature = "advanced_code_search"
	// FeatureContributionPrivacy gates Pro-only contribution-graph
	// controls beyond the gh-parity master toggle: per-repo opt-outs
	// that let a Pro user exclude specific repos from their public
	// heatmap. PRO-EXT01-09. Kinds: user only. The existing
	// users.include_private_contributions toggle remains Free.
	FeatureContributionPrivacy Feature = "contribution_privacy"
	// FeatureSecretScanHistory gates the Pro-only historical secret
	// scan across a user's repos. The on-demand scan worker consults
	// the repo owner's entitlement before doing any work. PRO-EXT01-10b.
	// Kinds: user only.
	FeatureSecretScanHistory Feature = "secret_scan_history"
	// FeatureSecretScanAlerts gates per-user email + webhook
	// notifications when a new secret-scan finding is recorded.
	// Companion to FeatureSecretScanHistory (a Free user has no
	// historical scan, so alerts are moot); separate constant so the
	// soak window for alert delivery can diverge — alert misroutes
	// are a data-leak risk worth observing independently before
	// flipping enforce.
	// Kinds: user only. PRO-EXT01-10d.
	FeatureSecretScanAlerts Feature = "secret_scan_alerts" // #nosec G101 -- entitlement feature key, not a credential.
	// FeatureFineGrainedPATs gates Pro-only PAT extensions: IP
	// allowlists (PRO-EXT01-11a), single-repo binding (PRO-EXT01-11b),
	// and — when shipped — custom-expiry / fine-grained scope syntax.
	// SECURITY-CRITICAL: the gate enforces the *write* path (a Free
	// user can't attach an IP allowlist to their token), but the
	// middleware-side enforcement of the allowlist itself ALWAYS
	// runs against any token that has one. The campaign enforce flag
	// gates *writing* the restriction, not honoring existing ones.
	// 14-day soak before PRO-EXT01-17 flip (longer than the 7-day
	// default).
	FeatureFineGrainedPATs Feature = "fine_grained_pats"
	// FeatureUserActionsSecrets gates personal Actions secrets +
	// variables (PRO-EXT01-12). User-scope rows share the
	// workflow_secrets and actions_variables tables with the existing
	// repo/org scopes; the gate exists at the *write* path (Free users
	// can't create user-scoped rows) and at runner-side resolution
	// (Free users' workflows skip the user scope on lookup).
	//
	// SECURITY-CRITICAL — runner secret resolution is a credential-
	// boundary surface. Ship in report-only (write gated, read returns
	// empty) before PRO-EXT01-17 flips the enforce flag.
	// Kinds: user only.
	FeatureUserActionsSecrets Feature = "user_actions_secrets"
	// FeatureUserActionsVariables is the variables sibling of
	// FeatureUserActionsSecrets (PRO-EXT01-12c). Same Pro-only
	// semantics; same surfaces (CRUD UI + runner-side resolution).
	// Mirrors the org-tier split between FeatureActionsOrgSecrets and
	// FeatureActionsOrgVariables so the per-feature soak windows can
	// diverge if needed.
	// Kinds: user only.
	FeatureUserActionsVariables Feature = "user_actions_variables"
	// FeatureWebhookRelay gates a Pro user's personal webhook relay
	// (PRO-EXT01-13a). A relay is an inbound endpoint that re-fans
	// upstream POSTs to N user-configured destination URLs with HMAC
	// signing. Gate fires at the receiver (deny inbound) AND at
	// create time (deny new relays). Free users under report-only
	// can still receive — the gate logs the would-deny so SREs can
	// see relay traffic shape before flipping enforce.
	//
	// SECURITY-CRITICAL — a relay is an amplification vector
	// (1 in → N out). Ship in report-only and run a security review
	// pass before PRO-EXT01-17 flips enforce.
	// Kinds: user only.
	FeatureWebhookRelay Feature = "webhook_relay"
	// FeatureCronWorkflowDispatch gates Pro-only scheduled workflow
	// dispatch — a per-account cron that fires workflow_dispatch
	// without committing a `schedule:` block (PRO-EXT01-13b).
	// Registered in 13a so the entitlements surface is complete; the
	// scheduler that consumes the gate ships in 13b.
	// Kinds: user only.
	FeatureCronWorkflowDispatch Feature = "cron_workflow_dispatch"
	// FeaturePersonalStatusPage gates a Pro user's personal status
	// page + SVG badge endpoint (PRO-EXT01-14). The page aggregates
	// Actions success rate across the user's pinned repos. Free users
	// see a teaser with placeholder data so the surface is
	// discoverable; the badge endpoint returns a 402-styled SVG.
	// Kinds: user only.
	FeaturePersonalStatusPage Feature = "personal_status_page"
	// FeatureRepoTimeMachine gates the Pro-only "paused" repo state
	// (PRO-EXT01-15). A paused repo keeps reads working but returns
	// 402 on every write surface (git push, REST mutations, issue/PR
	// creates). Useful for old side projects the owner doesn't want
	// to fully archive. A future sprint wires the per-repo storage
	// billing multiplier so paused repos bill at a reduced rate.
	// Kinds: user only.
	FeatureRepoTimeMachine Feature = "repo_time_machine"
	// FeatureInboxRules gates Pro-tier notification-routing rules
	// (PRO-EXT01-16a). A rule is a (match, action) pair: match on
	// reason/kind/repo/actor, action is snooze / tab / mark-read /
	// drop. Rules fire at fanout time after the inbox row is upserted.
	// Free users can't create rules; their existing rule rows (if any
	// from a downgrade) keep working in report-only mode until enforce
	// flips. Kinds: user only.
	FeatureInboxRules Feature = "inbox_rules"
	// FeatureInboxDigests gates Pro-tier scheduled digest emails
	// (PRO-EXT01-16b). A digest collapses the last window of unread
	// notifications into a single email at a user-chosen cadence
	// (daily or weekly). Free users can't enable the schedule; the
	// sweep worker skips any row whose owner's entitlement denies.
	// Kinds: user only.
	FeatureInboxDigests Feature = "inbox_digests"
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
	FreeOrgStorageQuotaBytes      int64 = 500 * 1024 * 1024
	TeamOrgStorageQuotaBytes      int64 = 2 * 1024 * 1024 * 1024
	FreeOrgActionsMinutesQuota    int64 = 2000
	TeamOrgActionsMinutesQuota    int64 = 3000

	// FreeProfilePinsCap mirrors gh's visible profile pin cap. Ratified
	// PRO01. Applies to Free users AND to all orgs — PRO07 leaves the
	// org cap as a hard constant since pins are a user-tier differentiator.
	FreeProfilePinsCap int64 = 6
	// ProProfilePinsCap is "effectively unlimited" for product copy, but
	// bounded for DB sanity per PRO01. A Pro user who pins 100 repos is
	// the upper bound — beyond that the request errors at the cap.
	ProProfilePinsCap int64 = 100

	// ProUsernameReservationsCap caps how many inactive handles a Pro
	// user can reserve against squatting. PRO-EXT01-05. Free is 0
	// (the FeatureUsernameReservations gate denies the write entirely).
	ProUsernameReservationsCap int64 = 3

	// FreeSavedRepliesCap is the Free-tier ceiling for saved replies.
	// Pro lifts the cap via FeatureSavedRepliesUnlimited; the absolute
	// Pro ceiling lives in ProSavedRepliesCap for DB sanity. PRO-EXT01-07a.
	FreeSavedRepliesCap int64 = 3
	// ProSavedRepliesCap bounds the "unlimited" upper edge to keep one
	// runaway client from filling the table. Quoted as "effectively
	// unlimited" in product copy; a Pro user who hits 500 has a script
	// that's misbehaving.
	ProSavedRepliesCap int64 = 500

	LimitSavedRepliesFreeCap Limit = "saved_replies_free_cap"
	LimitSavedRepliesProCap  Limit = "saved_replies_pro_cap"

	LimitPrivateCollaboration Limit = "private_collaboration_limit"
	LimitStorageQuota         Limit = "storage_quota"
	LimitActionsMinutesQuota  Limit = "actions_minutes_quota"
	// PRO07: profile-pin cap. Two limit keys are exposed so callers can
	// ask either "what's my current cap" (Set.Limit resolves to free or
	// pro based on plan) or "what's the absolute Free/Pro number." The
	// usual handler-side path queries Set.Limit(LimitProfilePinsFreeCap)
	// for the entitled cap; tests and the upgrade banner copy use the
	// Pro variant.
	LimitProfilePinsFreeCap Limit = "profile_pins_free_cap"
	LimitProfilePinsProCap  Limit = "profile_pins_pro_cap"
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
	FeatureProfilePinsBeyondFree:    {billing.SubjectKindUser},
	FeatureCodeOwnersReview:         {billing.SubjectKindUser, billing.SubjectKindOrg},
	FeatureProfileVanity:            {billing.SubjectKindUser},
	FeatureAnimatedAvatars:          {billing.SubjectKindUser},
	FeatureUsernameReservations:     {billing.SubjectKindUser},
	FeaturePrivateRepoTemplates:     {billing.SubjectKindUser},
	FeatureSavedRepliesUnlimited:    {billing.SubjectKindUser},
	FeatureScheduledIssues:          {billing.SubjectKindUser},
	FeatureAdvancedCodeSearch:       {billing.SubjectKindUser},
	FeatureContributionPrivacy:      {billing.SubjectKindUser},
	FeatureSecretScanHistory:        {billing.SubjectKindUser},
	FeatureSecretScanAlerts:         {billing.SubjectKindUser},
	FeatureFineGrainedPATs:          {billing.SubjectKindUser},
	FeatureUserActionsSecrets:       {billing.SubjectKindUser},
	FeatureUserActionsVariables:     {billing.SubjectKindUser},
	FeatureWebhookRelay:             {billing.SubjectKindUser},
	FeatureCronWorkflowDispatch:     {billing.SubjectKindUser},
	FeaturePersonalStatusPage:       {billing.SubjectKindUser},
	FeatureRepoTimeMachine:          {billing.SubjectKindUser},
	FeatureInboxRules:               {billing.SubjectKindUser},
	FeatureInboxDigests:             {billing.SubjectKindUser},
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
	Overridden   bool
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
	OrgID          int64             // populated for org kind only (deprecated; prefer Principal.ID)
	Principal      billing.Principal // PRO05: canonical routing key
	State          billing.State     // org-kind billing state (zero for user kind)
	UserState      billing.UserState // user-kind billing state (zero for org kind)
	now            time.Time
	quotaOverrides map[billing.QuotaKind]billing.QuotaOverride
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
		overrides, err := billing.ListOrgQuotaOverrides(ctx, bd, p.ID)
		if err != nil {
			return Set{}, err
		}
		quotaOverrides := make(map[billing.QuotaKind]billing.QuotaOverride, len(overrides))
		for _, override := range overrides {
			quotaOverrides[override.Kind] = override
		}
		return Set{OrgID: p.ID, Principal: p, State: state, now: now, quotaOverrides: quotaOverrides}, nil
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
	if s.Principal.IsOrg() && (name == LimitOrgStorageQuota || name == LimitOrgActionsMinutesQuota) {
		return s.usageLimit(name, feature, unit), nil
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
	// Profile pin caps are concrete on both sides of the gate: the Free
	// cap and Pro cap are constants ratified by PRO01, so the value is
	// always Defined regardless of the principal's plan. Handlers ask
	// CanUse(FeatureProfilePinsBeyondFree) to pick which cap applies.
	switch name {
	case LimitProfilePinsFreeCap:
		value.Defined = true
		value.Value = FreeProfilePinsCap
		return value, nil
	case LimitProfilePinsProCap:
		value.Defined = true
		value.Value = ProProfilePinsCap
		return value, nil
	}
	if name == LimitOrgPrivateCollaboration {
		value.Defined = true
		if decision.Allowed {
			value.Unlimited = true
		} else {
			value.Value = FreePrivateCollaborationLimit
		}
		return value, nil
	}
	if !decision.Allowed {
		return value, nil
	}
	switch name {
	case LimitOrgStorageQuota, LimitOrgActionsMinutesQuota:
		value = s.usageLimit(name, feature, unit)
	}
	return value, nil
}

func (s Set) usageLimit(name Limit, feature Feature, unit string) LimitValue {
	value := LimitValue{
		Name:         name,
		Feature:      feature,
		Allowed:      true,
		Defined:      true,
		Unit:         unit,
		RequiredPlan: billing.PlanTeam,
		Reason:       ReasonNone,
	}
	if s.State.Plan == billing.PlanEnterprise {
		value.Allowed = false
		value.Defined = false
		value.Reason = ReasonEnterpriseContactSales
		return value
	}
	teamActive := activeTeamBilling(s.now, s.State)
	switch name {
	case LimitOrgStorageQuota:
		value.Value = FreeOrgStorageQuotaBytes
		if teamActive {
			value.Value = TeamOrgStorageQuotaBytes
		}
	case LimitOrgActionsMinutesQuota:
		value.Value = FreeOrgActionsMinutesQuota
		if teamActive {
			value.Value = TeamOrgActionsMinutesQuota
		}
	}
	if override, ok := s.quotaOverrideForLimit(name); ok {
		value.Allowed = true
		value.Defined = true
		value.Overridden = true
		value.Reason = ReasonNone
		if override.Unlimited {
			value.Unlimited = true
			value.Value = 0
			return value
		}
		value.Unlimited = false
		if override.LimitValue.Valid {
			value.Value = override.LimitValue.Int64
		}
	}
	return value
}

func (s Set) quotaOverrideForLimit(name Limit) (billing.QuotaOverride, bool) {
	if len(s.quotaOverrides) == 0 {
		return billing.QuotaOverride{}, false
	}
	var kind billing.QuotaKind
	switch name {
	case LimitOrgStorageQuota:
		kind = billing.QuotaKindStorageBytes
	case LimitOrgActionsMinutesQuota:
		kind = billing.QuotaKindActionsMinutes
	default:
		return billing.QuotaOverride{}, false
	}
	override, ok := s.quotaOverrides[kind]
	return override, ok
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
	case LimitProfilePinsFreeCap, LimitProfilePinsProCap:
		return FeatureProfilePinsBeyondFree, "pins", true
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
		if activeTeamBilling(now, state) {
			decision.Allowed = true
			decision.Reason = ReasonNone
			return decision
		}
		decision.Reason = ReasonBillingActionNeeded
		return decision
	default:
		return decision
	}
}

func activeTeamBilling(now time.Time, state billing.State) bool {
	if state.Plan != billing.PlanTeam {
		return false
	}
	switch state.SubscriptionStatus {
	case billing.SubscriptionStatusActive,
		billing.SubscriptionStatusTrialing:
		return true
	case billing.SubscriptionStatusPastDue:
		return state.GraceUntil.Valid && !now.After(state.GraceUntil.Time)
	default:
		return false
	}
}
