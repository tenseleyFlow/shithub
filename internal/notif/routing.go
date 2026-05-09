// SPDX-License-Identifier: AGPL-3.0-or-later

package notif

// Reason is the short string the inbox row + email footer surface
// to the user ("you were mentioned", "you were assigned", etc.).
// Kept short because it's also the
// `notification_email_log.notification_id`-joined column on the
// inbox row.
type Reason string

const (
	ReasonMention         Reason = "mention"
	ReasonAssignment      Reason = "assignment"
	ReasonReviewRequested Reason = "review_requested"
	ReasonAuthor          Reason = "author"          // you opened the thread
	ReasonCommenter       Reason = "commenter"       // you commented earlier
	ReasonSubscribed      Reason = "subscribed"      // explicit thread sub
	ReasonWatching        Reason = "watching"        // repo-level watch
	ReasonRepoAdminAction Reason = "repo_admin_action"
)

// Relation is how a recipient relates to an event. The routing
// matrix is keyed on (event-kind, relation). Typically more than
// one relation applies — the fan-out worker picks the highest-
// priority one (the order matches the priority).
type Relation int

const (
	RelMention Relation = iota
	RelAssignee
	RelReviewer
	RelAuthor
	RelCommenter
	RelSubscribedThread
	RelWatchingAll
	RelWatchingParticipating
	RelRepoOwner // for repo-admin lifecycle events
)

// Action is the routing-matrix output. NotifyInbox controls the
// in-app inbox row; EmailDefault is whether to email by default
// (subject to the user's per-kind email pref). OverrideIgnore
// means the event notifies even when the user has set
// `watches.level = 'ignore'` for the repo (matches GitHub's
// "@mentions always notify" semantics).
type Action struct {
	NotifyInbox    bool
	EmailDefault   bool
	OverrideIgnore bool
	Reason         Reason
}

// Skip is the zero-action used when the matrix decides the
// event-relation pair shouldn't notify.
var Skip = Action{}

// Routing returns the action for an (event-kind, recipient
// relation) pair. The matrix is small enough today to be a
// switch; once we have ~30 event kinds it'll graduate to a real
// table-driven shape.
//
// Unknown kinds → Skip (deny by default — better to miss a
// notification than to spam users when a new kind is added but
// not routed).
func Routing(kind string, rel Relation) Action {
	switch kind {
	case "issue_opened", "pull_opened":
		// New issue / PR. Watching=all surfaces; nothing else.
		switch rel {
		case RelWatchingAll:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonWatching}
		}
	case "issue_comment", "pull_comment":
		switch rel {
		case RelMention:
			return Action{NotifyInbox: true, EmailDefault: true, OverrideIgnore: true, Reason: ReasonMention}
		case RelAssignee:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAssignment}
		case RelAuthor:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAuthor}
		case RelCommenter:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonCommenter}
		case RelSubscribedThread:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonSubscribed}
		case RelWatchingAll:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonWatching}
		}
	case "issue_assigned", "pull_assigned":
		switch rel {
		case RelAssignee:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAssignment}
		case RelMention:
			return Action{NotifyInbox: true, EmailDefault: true, OverrideIgnore: true, Reason: ReasonMention}
		}
	case "issue_closed", "pull_closed", "pull_merged":
		switch rel {
		case RelAuthor:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAuthor}
		case RelAssignee:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAssignment}
		case RelSubscribedThread:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonSubscribed}
		case RelWatchingAll:
			return Action{NotifyInbox: true, EmailDefault: false, Reason: ReasonWatching}
		}
	case "review_requested":
		switch rel {
		case RelReviewer:
			return Action{NotifyInbox: true, EmailDefault: true, OverrideIgnore: true, Reason: ReasonReviewRequested}
		}
	case "review_submitted":
		switch rel {
		case RelAuthor:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonAuthor}
		case RelSubscribedThread:
			return Action{NotifyInbox: true, EmailDefault: false, Reason: ReasonSubscribed}
		}
	case "mentioned":
		// Standalone mention event (no inline thread context).
		// Always inbox + email; overrides ignore.
		switch rel {
		case RelMention:
			return Action{NotifyInbox: true, EmailDefault: true, OverrideIgnore: true, Reason: ReasonMention}
		}
	case "check_failed", "check_fixed":
		// PR-author gets pinged when their PR's checks flip.
		switch rel {
		case RelAuthor:
			return Action{NotifyInbox: true, EmailDefault: false, Reason: ReasonAuthor}
		}
	case "repo_archived", "repo_unarchived",
		"repo_visibility_changed", "repo_soft_deleted",
		"repo_restored", "repo_transfer_requested",
		"repo_transfer_accepted", "repo_transfer_declined",
		"repo_transfer_canceled", "repo_transfer_expired":
		// S16-deferred lifecycle email kinds. Repo owner is the
		// canonical recipient; transfer flows additionally notify
		// the prospective recipient (computed by the caller).
		switch rel {
		case RelRepoOwner:
			return Action{NotifyInbox: true, EmailDefault: true, Reason: ReasonRepoAdminAction}
		}
	}
	return Skip
}

// EmailPrefKey returns the user_notification_prefs key for the
// per-kind email toggle. Empty means "no toggle exists for this
// reason — fall back to global default true."
func EmailPrefKey(reason Reason) string {
	switch reason {
	case ReasonMention:
		return "mentions_email"
	case ReasonAssignment:
		return "assignments_email"
	case ReasonReviewRequested:
		return "pr_review_requests_email"
	case ReasonAuthor, ReasonCommenter, ReasonSubscribed, ReasonWatching:
		return "issues_email"
	case ReasonRepoAdminAction:
		return "repo_admin_action_email"
	}
	return ""
}
