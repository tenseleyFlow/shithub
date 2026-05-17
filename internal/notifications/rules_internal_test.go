// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

// TestMatches table-drives the AND-semantics of the match filter set.
// Each match column is independent; NULL means "no filter on this
// dimension." Every cell of this matrix codifies one product
// promise — break any of them and a Pro user's expectations break.
func TestMatches(t *testing.T) {
	t.Parallel()

	type tc struct {
		name string
		rule notifdb.UserNotificationRule
		notf notifdb.Notification
		want bool
	}
	cases := []tc{
		{
			name: "empty rule matches everything",
			rule: notifdb.UserNotificationRule{},
			notf: notifdb.Notification{Reason: "mention", Kind: "issue_comment_created"},
			want: true,
		},
		{
			name: "reason matches",
			rule: notifdb.UserNotificationRule{
				MatchReason: pgtype.Text{String: "mention", Valid: true},
			},
			notf: notifdb.Notification{Reason: "mention"},
			want: true,
		},
		{
			name: "reason mismatch",
			rule: notifdb.UserNotificationRule{
				MatchReason: pgtype.Text{String: "mention", Valid: true},
			},
			notf: notifdb.Notification{Reason: "subscribed"},
			want: false,
		},
		{
			name: "kind + reason both required",
			rule: notifdb.UserNotificationRule{
				MatchReason: pgtype.Text{String: "mention", Valid: true},
				MatchKind:   pgtype.Text{String: "issue_comment_created", Valid: true},
			},
			notf: notifdb.Notification{Reason: "mention", Kind: "issue_comment_created"},
			want: true,
		},
		{
			name: "kind required but kind mismatched",
			rule: notifdb.UserNotificationRule{
				MatchReason: pgtype.Text{String: "mention", Valid: true},
				MatchKind:   pgtype.Text{String: "review_requested", Valid: true},
			},
			notf: notifdb.Notification{Reason: "mention", Kind: "issue_comment_created"},
			want: false,
		},
		{
			name: "repo filter — match",
			rule: notifdb.UserNotificationRule{
				MatchRepoID: pgtype.Int8{Int64: 100, Valid: true},
			},
			notf: notifdb.Notification{
				RepoID: pgtype.Int8{Int64: 100, Valid: true},
			},
			want: true,
		},
		{
			name: "repo filter — mismatch",
			rule: notifdb.UserNotificationRule{
				MatchRepoID: pgtype.Int8{Int64: 100, Valid: true},
			},
			notf: notifdb.Notification{
				RepoID: pgtype.Int8{Int64: 200, Valid: true},
			},
			want: false,
		},
		{
			name: "repo filter — notification has no repo (lifecycle event)",
			rule: notifdb.UserNotificationRule{
				MatchRepoID: pgtype.Int8{Int64: 100, Valid: true},
			},
			notf: notifdb.Notification{},
			want: false,
		},
		{
			name: "actor filter — match",
			rule: notifdb.UserNotificationRule{
				MatchActorID: pgtype.Int8{Int64: 7, Valid: true},
			},
			notf: notifdb.Notification{
				LastActorUserID: pgtype.Int8{Int64: 7, Valid: true},
			},
			want: true,
		},
		{
			name: "actor filter — notification has no actor",
			rule: notifdb.UserNotificationRule{
				MatchActorID: pgtype.Int8{Int64: 7, Valid: true},
			},
			notf: notifdb.Notification{},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := matches(c.rule, c.notf); got != c.want {
				t.Errorf("matches = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBuildDecision_SnoozeUsesEvaluatorClock confirms snooze
// timestamps come from the Evaluator's overridable Now, so the
// fanout-integration test can pin them deterministically.
func TestBuildDecision_SnoozeUsesEvaluatorClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	e := &Evaluator{Now: func() time.Time { return fixed }}
	rule := notifdb.UserNotificationRule{
		ID:                  1,
		Action:              notifdb.UserNotificationRuleActionSnooze,
		ActionSnoozeMinutes: pgtype.Int4{Int32: 30, Valid: true},
	}
	d := e.buildDecision(rule)
	want := fixed.Add(30 * time.Minute)
	if !d.SnoozeUntil.Equal(want) {
		t.Errorf("SnoozeUntil = %v, want %v", d.SnoozeUntil, want)
	}
	if d.RuleID != 1 {
		t.Errorf("RuleID = %d, want 1", d.RuleID)
	}
}

// TestBuildDecision_TabPullsLabel: the tab action carries the
// caller-supplied label through verbatim.
func TestBuildDecision_TabPullsLabel(t *testing.T) {
	t.Parallel()
	e := &Evaluator{}
	rule := notifdb.UserNotificationRule{
		ID:        42,
		Action:    notifdb.UserNotificationRuleActionTab,
		ActionTab: pgtype.Text{String: "security", Valid: true},
	}
	d := e.buildDecision(rule)
	if d.Tab != "security" {
		t.Errorf("Tab = %q, want %q", d.Tab, "security")
	}
	if d.RuleID != 42 {
		t.Errorf("RuleID = %d, want 42", d.RuleID)
	}
}
