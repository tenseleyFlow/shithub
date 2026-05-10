// SPDX-License-Identifier: AGPL-3.0-or-later

// Package trigger evaluates whether a parsed workflow's `on:` block
// accepts a given triggering event, and (when matched) builds the
// `workflow_runs` row plus its child jobs/steps in one transaction.
//
// The package is split into three concerns:
//
//   - event.go:     the typed Event input shape
//   - glob.go:      pattern matching (branches/tags/paths filters)
//   - match.go:     pure Match(workflow, event) → bool
//   - discover.go:  git-tree walk to find .shithub/workflows/*.yml at a SHA
//   - enqueue.go:   one-tx insertion of a matched workflow's run+jobs+steps
//
// Match is pure (no I/O, no DB) so it's cheap to fuzz and table-test.
// Discover and Enqueue do I/O but stay free of business logic.
package trigger

// EventKind enumerates the four triggering event types v1 supports.
// Anything else (release, deployment, repository_dispatch, …) is parked
// for v2 — Match will return false for unrecognized kinds rather than
// erroring, mirroring the rest of the v1 strict-allowlist posture.
type EventKind string

const (
	EventPush             EventKind = "push"
	EventPullRequest      EventKind = "pull_request"
	EventSchedule         EventKind = "schedule"
	EventWorkflowDispatch EventKind = "workflow_dispatch"
)

// Event is the typed input the Match function consults. Constructed by
// the caller from the actual triggering source (push_event row, PR
// state transition, cron sweep, dispatch HTTP body) before invoking
// the trigger pipeline.
//
// Fields are populated only when relevant to the event kind:
//
//   - Push:             Ref, Branch, Tag, ChangedPaths
//   - PullRequest:      Action, BaseRef, HeadRef, ChangedPaths
//   - Schedule:         Cron (the expression that fired)
//   - WorkflowDispatch: (no extra fields)
//
// Match never reads a field not relevant to its Kind, so leaving the
// other fields zero is correct.
type Event struct {
	// Kind selects which `on:` predicate to evaluate.
	Kind EventKind

	// Ref is the full ref name (e.g. "refs/heads/main", "refs/tags/v1.0.0").
	// Used for push events to drive Branch/Tag classification.
	Ref string

	// Branch is the short branch name (the part after refs/heads/) when
	// the event is a push to a branch. Empty for tag pushes.
	Branch string

	// Tag is the short tag name (the part after refs/tags/) when the
	// event is a push to a tag. Empty for branch pushes.
	Tag string

	// Action is the activity-type qualifier on a pull_request event:
	// "opened", "synchronize", "reopened", "closed", "edited", "labeled",
	// "unlabeled". v1 doesn't constrain the value; the emitter is the
	// allowlist.
	Action string

	// BaseRef is the destination branch of a pull_request (the short
	// name, e.g. "main"). Used by pull_request branches: filter.
	BaseRef string

	// HeadRef is the source branch of a pull_request (the short name).
	HeadRef string

	// ChangedPaths is the list of repo-relative paths touched by the
	// event. Drives the paths: filter on push and pull_request.
	// Order doesn't matter; uniqueness isn't required.
	ChangedPaths []string

	// Cron is the cron expression that fired (e.g. "*/5 * * * *"). Used
	// to attribute a schedule trigger to a specific schedule entry on
	// the workflow when the workflow has multiple `on.schedule` lines.
	Cron string
}
