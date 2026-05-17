// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notifications implements the Pro-tier rule-based routing
// engine (PRO-EXT01-16a). A Rule is a (match, action) pair scoped to
// one user; the fanout side loads the user's enabled rules, evaluates
// them in position order against a freshly-upserted Notification, and
// applies the first matching action.
//
// The rule engine deliberately does NOT mutate the notification row
// in-place — that's the caller's responsibility (the fanout
// integration uses one of the ApplyRule* sqlc queries). Keeping the
// matcher pure makes it cheap to unit-test against synthetic events
// without spinning up Postgres.
package notifications

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

// Decision is the result of evaluating a user's rules against one
// notification. RuleID == 0 means no rule matched and the
// notification should be left untouched.
//
// Exactly one of the action fields is meaningful per Decision:
//   - SnoozeUntil set + Action="snooze"
//   - Tab set + Action="tab"
//   - Action="mark_read" (no params)
//   - Action="drop" (no params)
type Decision struct {
	RuleID      int64
	Action      notifdb.UserNotificationRuleAction
	SnoozeUntil time.Time
	Tab         string
}

// Evaluator is the side of the engine the fanout calls. Construct
// once at process start.
type Evaluator struct {
	Pool *pgxpool.Pool
	// Logger is used to emit `entitlements.report_only_deny` lines
	// when a Free user's rules fire under report-only mode. Nil
	// disables that telemetry without disabling the engine itself.
	Logger *slog.Logger
	// EnforceFree, when true, skips rule evaluation entirely for
	// recipients whose entitlements forbid FeatureInboxRules. Off
	// (the default) keeps the engine running for Free users and just
	// logs the would-deny — the soak path for PRO-EXT01-17.
	EnforceFree bool
	// Now is overridable in tests to make snooze calculations
	// deterministic. Production passes nil → time.Now().
	Now func() time.Time
}

// Evaluate loads the user's enabled rules and returns the first match
// for the given notification, or a zero Decision (RuleID == 0) if no
// rule fires.
//
// Order of operations:
//  1. Load enabled rules. Short-circuit if the user has none — keeps
//     the entitlement query off the hot path for users without rules.
//  2. Check the user's FeatureInboxRules entitlement. Free + enforce →
//     return zero. Free + report-only → log + continue.
//  3. Iterate rules in position order; first match wins.
func (e *Evaluator) Evaluate(ctx context.Context, n notifdb.Notification) (Decision, error) {
	if e == nil || e.Pool == nil {
		return Decision{}, nil
	}
	rules, err := notifdb.New().ListEnabledUserNotificationRules(ctx, e.Pool, n.RecipientUserID)
	if err != nil {
		return Decision{}, err
	}
	if len(rules) == 0 {
		return Decision{}, nil
	}
	if !e.allowedForRecipient(ctx, n.RecipientUserID) {
		return Decision{}, nil
	}
	for _, rule := range rules {
		if !matches(rule, n) {
			continue
		}
		return e.buildDecision(rule), nil
	}
	return Decision{}, nil
}

// allowedForRecipient runs the entitlement gate. Returns true when
// rule evaluation should proceed (Pro user, or Free under report-
// only). False blocks evaluation entirely.
//
// On error the function fails open under report-only and closed
// under enforce — same shape every other gate uses so an
// entitlement-DB blip doesn't accidentally unlock or lock the
// feature.
func (e *Evaluator) allowedForRecipient(ctx context.Context, recipientID int64) bool {
	principal := billing.PrincipalForUser(recipientID)
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: e.Pool}, principal, entitlements.FeatureInboxRules)
	if err != nil {
		return !e.EnforceFree
	}
	if decision.Allowed {
		return true
	}
	if e.Logger != nil {
		mode := "report_only"
		if e.EnforceFree {
			mode = "enforce"
		}
		e.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", recipientID,
			"feature", string(entitlements.FeatureInboxRules),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "notif-fanout")
	}
	return !e.EnforceFree
}

// matches is the pure rule-matcher. Each match column is treated as
// an AND filter; NULL means "no filter on this dimension." A rule
// with all NULL match columns matches every notification (the user
// asked for "catch-all").
func matches(r notifdb.UserNotificationRule, n notifdb.Notification) bool {
	if r.MatchReason.Valid && r.MatchReason.String != n.Reason {
		return false
	}
	if r.MatchKind.Valid && r.MatchKind.String != n.Kind {
		return false
	}
	if r.MatchRepoID.Valid {
		if !n.RepoID.Valid || r.MatchRepoID.Int64 != n.RepoID.Int64 {
			return false
		}
	}
	if r.MatchActorID.Valid {
		if !n.LastActorUserID.Valid || r.MatchActorID.Int64 != n.LastActorUserID.Int64 {
			return false
		}
	}
	return true
}

func (e *Evaluator) buildDecision(r notifdb.UserNotificationRule) Decision {
	d := Decision{RuleID: r.ID, Action: r.Action}
	switch r.Action {
	case notifdb.UserNotificationRuleActionSnooze:
		if r.ActionSnoozeMinutes.Valid {
			d.SnoozeUntil = e.now().Add(time.Duration(r.ActionSnoozeMinutes.Int32) * time.Minute)
		}
	case notifdb.UserNotificationRuleActionTab:
		if r.ActionTab.Valid {
			d.Tab = r.ActionTab.String
		}
	}
	return d
}

func (e *Evaluator) now() time.Time {
	if e == nil || e.Now == nil {
		return time.Now()
	}
	return e.Now()
}
