// SPDX-License-Identifier: AGPL-3.0-or-later

package notif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// FanoutConsumer is the consumer name used in
// `domain_events_processed`. Other consumers (S33 webhooks) will
// use distinct names so cursors don't collide.
const FanoutConsumer = "notify_fanout"

// FanoutOnce drains up to FanoutBatch domain_events starting after
// the persisted cursor, computes recipient sets, and writes inbox
// rows + (optionally) emails. Returns the number of events
// processed so the caller can decide to loop.
//
// Designed to be invoked from a worker job (see
// `internal/worker/jobs/notify_fanout.go`); the cron framework's
// landing date will determine how it's scheduled (the job kind is
// already in the worker registry).
func FanoutOnce(ctx context.Context, deps Deps) (int, error) {
	q := notifdb.New()
	cur, err := q.GetEventCursor(ctx, deps.Pool, FanoutConsumer)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("fanout: load cursor: %w", err)
	}
	last := int64(0)
	if err == nil {
		last = cur.LastEventID
	}

	rows, err := q.ListUnprocessedDomainEvents(ctx, deps.Pool, notifdb.ListUnprocessedDomainEventsParams{
		ID:    last,
		Limit: int32(FanoutBatch),
	})
	if err != nil {
		return 0, fmt.Errorf("fanout: list events: %w", err)
	}

	processed := 0
	for _, ev := range rows {
		if err := dispatchEvent(ctx, deps, ev); err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "fanout: dispatch failed",
					"event_id", ev.ID, "kind", ev.Kind, "error", err)
			}
			// Don't advance the cursor past a failure — retry on
			// the next tick. (A poison row would loop forever; the
			// per-event error handling below catches the typical
			// poison shapes and skips them.)
			break
		}
		last = ev.ID
		processed++
	}

	if processed > 0 {
		if err := q.SetEventCursor(ctx, deps.Pool, notifdb.SetEventCursorParams{
			Consumer: FanoutConsumer, LastEventID: last,
		}); err != nil {
			return processed, fmt.Errorf("fanout: persist cursor: %w", err)
		}
	}
	return processed, nil
}

// dispatchEvent routes a single domain_events row through the
// recipient computer + per-recipient action engine.
func dispatchEvent(ctx context.Context, deps Deps, ev notifdb.DomainEvent) error {
	actorUserID := int64(0)
	if ev.ActorUserID.Valid {
		actorUserID = ev.ActorUserID.Int64
	}
	repoID := int64(0)
	if ev.RepoID.Valid {
		repoID = ev.RepoID.Int64
	}

	threadKind, threadID, err := threadFromEvent(ctx, deps, ev)
	if err != nil {
		return err
	}

	// Compute recipient set. Each recipient is paired with the
	// relation that earned them the slot; the routing matrix
	// decides what to do per (kind, relation).
	recipients, err := computeRecipients(ctx, deps, ev, threadKind, threadID, actorUserID, repoID)
	if err != nil {
		return err
	}

	// Auto-subscription: insert an "if-absent" thread row for
	// users whose participation rule earned them this notification
	// (author / assignee / reviewer / mentioned / commenter). Their
	// future notifications then route via the subscribed-thread
	// relation without re-deriving the rule.
	if threadID != 0 {
		applyAutoSubscriptions(ctx, deps, ev, threadKind, threadID, recipients)
	}

	q := notifdb.New()
	for recipID, rec := range recipients {
		// Self-suppression: never notify the actor about their own
		// action. Defense in depth — the recipient computer also
		// excludes them, but the duplicate guard is cheap.
		if recipID == actorUserID {
			continue
		}
		// Visibility re-check at fan-out time. The repo could have
		// flipped public→private between event-emit and fan-out;
		// a stale-read public event must not leak.
		if repoID != 0 {
			ok, err := canRecipientSeeRepo(ctx, deps, recipID, repoID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		}
		action := pickAction(ev.Kind, rec.Relations)
		if !action.NotifyInbox {
			continue
		}
		// `OverrideIgnore=false` AND user has watches.level=ignore
		// on the repo → skip. The OverrideIgnore=true case (i.e.
		// @mentions) bypasses the ignore check.
		if !action.OverrideIgnore && repoID != 0 {
			ignored, err := repoIgnoredByRecipient(ctx, deps, recipID, repoID)
			if err != nil {
				return err
			}
			if ignored {
				continue
			}
		}
		// Materialize the inbox row.
		notif, err := upsertInboxRow(ctx, deps, q, recipID, ev, action.Reason, threadKind, threadID, repoID, actorUserID)
		if err != nil {
			return err
		}
		// PRO-EXT01-16a: evaluate the recipient's routing rules and
		// stamp the decision onto the row. applyRuleDecision returns
		// the post-mutation notification so the email path sees the
		// snooze/drop state.
		notif, err = applyRuleDecision(ctx, deps, q, notif)
		if err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "fanout: rule decision",
				"recipient", recipID, "notification_id", notif.ID, "error", err)
		}
		// Email side. Pref + storm dampener gate. Snoozed/dropped
		// notifications skip the email — that's the whole point of
		// the focus-mode feature.
		if shouldEmail(action, notif) && deps.EmailSender != nil {
			if err := maybeSendEmail(ctx, deps, q, recipID, notif, action.Reason, threadKind, threadID); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "fanout: email send",
					"recipient", recipID, "error", err)
			}
		}
	}
	return nil
}

// canRecipientSeeRepo runs the visibility predicate by hand for
// one recipient. Cheap — repo + collab-row lookup, always indexed.
func canRecipientSeeRepo(ctx context.Context, deps Deps, recipientID, repoID int64) (bool, error) {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	has2FA, err := usersdb.New().HasConfirmedUserTOTP(ctx, deps.Pool, recipientID)
	if err != nil {
		return false, err
	}
	actor := policy.UserActorWithTwoFactor(recipientID, "", false, false, has2FA)
	return policy.IsVisibleTo(ctx, policy.Deps{Pool: deps.Pool}, actor, policy.NewRepoRefFromRepo(repo)), nil
}

// repoIgnoredByRecipient reports whether the recipient has set
// `watches.level = 'ignore'` for the repo.
func repoIgnoredByRecipient(ctx context.Context, deps Deps, recipientID, repoID int64) (bool, error) {
	row, err := socialdb.New().GetWatch(ctx, deps.Pool, socialdb.GetWatchParams{
		UserID: recipientID, RepoID: repoID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return row.Level == socialdb.WatchLevelIgnore, nil
}

// upsertInboxRow either creates a fresh notifications row for a
// (recipient, thread) pair or coalesces onto the existing row by
// bumping last_event_at + last_actor + setting unread=true.
func upsertInboxRow(
	ctx context.Context, deps Deps, q *notifdb.Queries,
	recipientID int64,
	ev notifdb.DomainEvent,
	reason Reason,
	threadKind notifdb.NullNotificationThreadKind,
	threadID int64,
	repoID int64,
	actorUserID int64,
) (notifdb.Notification, error) {
	if threadID == 0 {
		// Thread-less notification (e.g. lifecycle). Each event
		// fires its own row; no coalesce.
		return q.InsertThreadlessNotification(ctx, deps.Pool, notifdb.InsertThreadlessNotificationParams{
			RecipientUserID: recipientID,
			Kind:            ev.Kind,
			Reason:          string(reason),
			RepoID:          intToPg(repoID),
			SourceEventID:   pgtype.Int8{Int64: ev.ID, Valid: true},
			LastActorUserID: intToPg(actorUserID),
		})
	}
	return q.UpsertNotificationByThread(ctx, deps.Pool, notifdb.UpsertNotificationByThreadParams{
		RecipientUserID: recipientID,
		Kind:            ev.Kind,
		Reason:          string(reason),
		RepoID:          intToPg(repoID),
		ThreadKind:      threadKind,
		ThreadID:        pgtype.Int8{Int64: threadID, Valid: true},
		SourceEventID:   pgtype.Int8{Int64: ev.ID, Valid: true},
		LastActorUserID: intToPg(actorUserID),
	})
}

// pickAction iterates the recipient's relations in priority order
// and returns the first non-Skip Action. The `RelMention` slot is
// most permissive (override-ignore) so it's first.
func pickAction(kind string, rels []Relation) Action {
	for _, rel := range rels {
		if a := Routing(kind, rel); a.NotifyInbox {
			return a
		}
	}
	return Skip
}

// threadFromEvent extracts the (thread_kind, thread_id) pair when
// the event is thread-shaped (issue or PR). For non-thread events
// (repo-admin lifecycle) returns ({Valid:false}, 0).
func threadFromEvent(ctx context.Context, deps Deps, ev notifdb.DomainEvent) (notifdb.NullNotificationThreadKind, int64, error) {
	switch ev.SourceKind {
	case "issue":
		// payload may carry kind=issue|pr; we resolve via the
		// issues table to be sure.
		issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, ev.SourceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notifdb.NullNotificationThreadKind{}, 0, nil
			}
			return notifdb.NullNotificationThreadKind{}, 0, err
		}
		kind := notifdb.NotificationThreadKindIssue
		if issue.Kind == issuesdb.IssueKindPr {
			kind = notifdb.NotificationThreadKindPr
		}
		return notifdb.NullNotificationThreadKind{NotificationThreadKind: kind, Valid: true}, issue.ID, nil
	}
	return notifdb.NullNotificationThreadKind{}, 0, nil
}

// recipient is one row in the per-event recipient set. Multiple
// relations may apply (e.g. user is both author + assignee); the
// pickAction step picks the highest-priority Action.
type recipient struct {
	Relations []Relation
}

func (r *recipient) add(rel Relation) {
	for _, existing := range r.Relations {
		if existing == rel {
			return
		}
	}
	r.Relations = append(r.Relations, rel)
}

// computeRecipients gathers users from every relevant slot:
// * mention list from the event payload (when applicable),
// * thread author / assignee / reviewer (for thread-shaped events),
// * watches at level=all/participating on the repo,
// * explicit thread subscribers,
// * repo owner (for lifecycle events).
//
// The actor is excluded; visibility re-check happens later.
func computeRecipients(
	ctx context.Context, deps Deps,
	ev notifdb.DomainEvent,
	threadKind notifdb.NullNotificationThreadKind,
	threadID int64,
	actorUserID, repoID int64,
) (map[int64]*recipient, error) {
	out := map[int64]*recipient{}

	// Mentions live in the payload as a `mentions` array of user ids.
	if ev.Payload != nil {
		var p struct {
			Mentions []int64 `json:"mentions"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		for _, uid := range p.Mentions {
			ensure(out, uid).add(RelMention)
		}
	}

	// Thread relations (author, assignee, commenter via auto-sub).
	if threadID != 0 {
		issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, threadID)
		if err == nil {
			if issue.AuthorUserID.Valid {
				ensure(out, issue.AuthorUserID.Int64).add(RelAuthor)
			}
			// Assignees from issue_assignees.
			assignees, err := issuesdb.New().ListIssueAssignees(ctx, deps.Pool, threadID)
			if err == nil {
				for _, a := range assignees {
					ensure(out, a.UserID).add(RelAssignee)
				}
			}
		}
		// Explicit thread subscribers.
		subs, err := notifdb.New().ListSubscribersForThread(ctx, deps.Pool, notifdb.ListSubscribersForThreadParams{
			ThreadKind: threadKind.NotificationThreadKind,
			ThreadID:   threadID,
		})
		if err == nil {
			for _, s := range subs {
				ensure(out, s.RecipientUserID).add(RelSubscribedThread)
			}
		}
	}

	// Repo watchers at level=all (relevant for "new issue" /
	// "new PR" events). The fan-out matrix decides which kinds
	// actually consume this slot — so populating it for every
	// thread-shaped event is fine (skipped at routing time).
	if repoID != 0 {
		watchers, err := socialdb.New().ListRepoWatchersByLevel(ctx, deps.Pool, socialdb.ListRepoWatchersByLevelParams{
			RepoID: repoID, Level: socialdb.WatchLevelAll,
		})
		if err == nil {
			for _, uid := range watchers {
				ensure(out, uid).add(RelWatchingAll)
			}
		}
	}

	// Repo-owner slot for lifecycle events.
	if repoID != 0 && isLifecycleEvent(ev.Kind) {
		repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, repoID)
		if err == nil && repo.OwnerUserID.Valid {
			ensure(out, repo.OwnerUserID.Int64).add(RelRepoOwner)
		}
	}

	// Reviewer slot for review_requested events. Payload carries
	// the recipient id.
	if ev.Kind == "review_requested" && ev.Payload != nil {
		var p struct {
			ReviewerUserID int64 `json:"reviewer_user_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.ReviewerUserID != 0 {
			ensure(out, p.ReviewerUserID).add(RelReviewer)
		}
	}

	// Self-suppression: drop the actor. We still defense-in-depth
	// it at the dispatch site, but trimming early saves per-
	// recipient policy lookups.
	if actorUserID != 0 {
		delete(out, actorUserID)
	}
	return out, nil
}

// applyAutoSubscriptions inserts notification_threads rows for the
// recipients that just earned a slot via author / assignee /
// reviewer / mention / first-commenter rules. Non-destructive —
// preserves explicit user choices.
func applyAutoSubscriptions(
	ctx context.Context, deps Deps,
	ev notifdb.DomainEvent,
	threadKind notifdb.NullNotificationThreadKind,
	threadID int64,
	recipients map[int64]*recipient,
) {
	q := notifdb.New()
	for uid, rec := range recipients {
		var reason string
		var subscribed bool
		// Pick the strongest auto-sub reason that applies.
		for _, rel := range rec.Relations {
			switch rel {
			case RelAuthor:
				reason, subscribed = "author", true
			case RelAssignee:
				reason, subscribed = "assigned", true
			case RelReviewer:
				reason, subscribed = "review_requested", true
			case RelMention:
				if reason == "" {
					reason, subscribed = "mention", true
				}
			case RelCommenter:
				if reason == "" {
					reason, subscribed = "commenter", true
				}
			}
		}
		if reason == "" {
			continue
		}
		_ = q.InsertNotificationThreadIfAbsent(ctx, deps.Pool, notifdb.InsertNotificationThreadIfAbsentParams{
			RecipientUserID: uid,
			ThreadKind:      threadKind.NotificationThreadKind,
			ThreadID:        threadID,
			Subscribed:      subscribed,
			Reason:          reason,
		})
	}
}

// maybeSendEmail consults the user's per-kind email pref + the
// storm dampener, then either dispatches the email or skips. Logs
// every attempt to notification_email_log so the dampener is
// self-consistent across worker restarts.
func maybeSendEmail(
	ctx context.Context, deps Deps, q *notifdb.Queries,
	recipientID int64,
	notif notifdb.Notification,
	reason Reason,
	threadKind notifdb.NullNotificationThreadKind,
	threadID int64,
) error {
	prefKey := EmailPrefKey(reason)
	if prefKey != "" {
		on, err := emailPrefOn(ctx, deps.Pool, recipientID, prefKey)
		if err != nil {
			return err
		}
		if !on {
			return nil
		}
	}

	// Per-thread dampener.
	if threadID != 0 {
		count, err := q.CountEmailsForRecipientThreadSince(ctx, deps.Pool, notifdb.CountEmailsForRecipientThreadSinceParams{
			RecipientUserID: recipientID,
			ThreadKind:      threadKind,
			ThreadID:        pgtype.Int8{Int64: threadID, Valid: true},
			Column4:         StormPerThreadMins,
		})
		if err != nil {
			return err
		}
		if count >= StormPerThreadCap {
			return nil
		}
	}
	// Per-recipient absolute cap.
	count, err := q.CountEmailsForRecipientSince(ctx, deps.Pool, notifdb.CountEmailsForRecipientSinceParams{
		RecipientUserID: recipientID,
		Column2:         StormAbsoluteMins,
	})
	if err != nil {
		return err
	}
	if count >= StormAbsoluteCap {
		return nil
	}

	// We don't actually render + send here — that's a separate
	// helper (`sendNotificationEmail`). Logging the send here is
	// a layering violation; the helper logs after the SMTP call
	// succeeds. The helper isn't ready in this commit; the gate
	// stays so the storm dampener is wired even if no template
	// renders today.
	return sendNotificationEmail(ctx, deps, recipientID, notif, reason, threadKind, threadID)
}

// ensure returns the *recipient for uid, creating it on first add.
func ensure(m map[int64]*recipient, uid int64) *recipient {
	if r, ok := m[uid]; ok {
		return r
	}
	r := &recipient{}
	m[uid] = r
	return r
}

func intToPg(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: v != 0}
}

// applyRuleDecision asks the rule engine what to do with the
// freshly-upserted notification, mutates the row via the appropriate
// ApplyRule* sqlc query, and returns the post-mutation row so the
// caller can branch on snooze/drop state.
//
// A nil engine, no matching rule, or any error → return the input
// notification unchanged so the fanout keeps making progress.
// Failures are logged but never escalate; rule evaluation is a
// best-effort enhancement on top of the existing fanout flow.
func applyRuleDecision(
	ctx context.Context, deps Deps, q *notifdb.Queries, n notifdb.Notification,
) (notifdb.Notification, error) {
	if deps.RuleEngine == nil {
		return n, nil
	}
	d, err := deps.RuleEngine.Evaluate(ctx, n)
	if err != nil {
		return n, err
	}
	if d.RuleID == 0 {
		return n, nil
	}
	ruleID := pgtype.Int8{Int64: d.RuleID, Valid: true}
	switch d.Action {
	case notifdb.UserNotificationRuleActionSnooze:
		if err := q.ApplyRuleSnooze(ctx, deps.Pool, notifdb.ApplyRuleSnoozeParams{
			ID:            n.ID,
			SnoozedUntil:  pgtype.Timestamptz{Time: d.SnoozeUntil, Valid: true},
			MatchedRuleID: ruleID,
		}); err != nil {
			return n, err
		}
		n.SnoozedUntil = pgtype.Timestamptz{Time: d.SnoozeUntil, Valid: true}
		n.MatchedRuleID = ruleID
	case notifdb.UserNotificationRuleActionTab:
		if err := q.ApplyRuleTab(ctx, deps.Pool, notifdb.ApplyRuleTabParams{
			ID:            n.ID,
			TabLabel:      pgtype.Text{String: d.Tab, Valid: true},
			MatchedRuleID: ruleID,
		}); err != nil {
			return n, err
		}
		n.TabLabel = pgtype.Text{String: d.Tab, Valid: true}
		n.MatchedRuleID = ruleID
	case notifdb.UserNotificationRuleActionMarkRead:
		if err := q.ApplyRuleMarkRead(ctx, deps.Pool, notifdb.ApplyRuleMarkReadParams{
			ID:            n.ID,
			MatchedRuleID: ruleID,
		}); err != nil {
			return n, err
		}
		n.Unread = false
		n.MatchedRuleID = ruleID
	case notifdb.UserNotificationRuleActionDrop:
		if err := q.ApplyRuleDrop(ctx, deps.Pool, notifdb.ApplyRuleDropParams{
			ID:            n.ID,
			MatchedRuleID: ruleID,
		}); err != nil {
			return n, err
		}
		n.TabLabel = pgtype.Text{String: "dropped", Valid: true}
		n.MatchedRuleID = ruleID
	}
	return n, nil
}

// shouldEmail decides whether the storm-dampener-gated email path
// should run, given the post-rule state of the notification. A
// snoozed or dropped row skips email — that's the user's stated
// preference via the rule.
func shouldEmail(action Action, n notifdb.Notification) bool {
	if !action.EmailDefault {
		return false
	}
	if n.SnoozedUntil.Valid {
		return false
	}
	if n.TabLabel.Valid && n.TabLabel.String == "dropped" {
		return false
	}
	return true
}

func isLifecycleEvent(kind string) bool {
	switch kind {
	case "repo_archived", "repo_unarchived",
		"repo_visibility_changed", "repo_soft_deleted",
		"repo_restored", "repo_transfer_requested",
		"repo_transfer_accepted", "repo_transfer_declined",
		"repo_transfer_canceled", "repo_transfer_expired":
		return true
	}
	return false
}
