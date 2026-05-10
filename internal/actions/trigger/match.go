// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import (
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

// Match reports whether the workflow's `on:` predicates accept the
// given event. Pure function: no I/O, no DB. Cheap to fuzz.
//
// The four event kinds:
//
//   - push              → on.push present, branch/tag classification
//     passes the appropriate sub-filter, paths
//     filter (when set) hits at least one
//     changed path
//   - pull_request      → on.pull_request present, action is in the
//     configured types: list (default ["opened",
//     "synchronize", "reopened"]), base branch
//     passes branches: filter, paths filter hits
//   - schedule          → on.schedule has any entry whose cron string
//     equals event.Cron (the sweep tells us which
//     cron fired; we just verify the workflow
//     declared it)
//   - workflow_dispatch → on.workflow_dispatch present
//
// Anything else returns false silently — strict-allowlist v1 posture.
//
// A workflow that doesn't declare the trigger kind at all (e.g.,
// only `on: push` but the event is a pull_request) returns false.
func Match(w *workflow.Workflow, ev Event) bool {
	if w == nil {
		return false
	}
	switch ev.Kind {
	case EventPush:
		return matchPush(w.On.Push, ev)
	case EventPullRequest:
		return matchPullRequest(w.On.PullRequest, ev)
	case EventSchedule:
		return matchSchedule(w.On.Schedule, ev)
	case EventWorkflowDispatch:
		return w.On.WorkflowDispatch != nil
	}
	return false
}

func matchPush(pt *workflow.PushTrigger, ev Event) bool {
	if pt == nil {
		return false
	}
	// Branch vs tag classification:
	//   - When event has a Branch set, only the branches: filter applies
	//     (a tags-only filter wouldn't accept a branch push, and vice
	//     versa). Match GHA semantics.
	//   - Same for Tag.
	switch {
	case ev.Branch != "":
		// If neither branches nor tags is configured, match-all.
		// If only tags is configured, this branch push doesn't match.
		if len(pt.Branches) == 0 && len(pt.Tags) == 0 {
			return matchPaths(pt.Paths, ev.ChangedPaths)
		}
		if len(pt.Branches) == 0 {
			return false
		}
		if !matchAny(pt.Branches, ev.Branch) {
			return false
		}
	case ev.Tag != "":
		if len(pt.Branches) == 0 && len(pt.Tags) == 0 {
			return matchPaths(pt.Paths, ev.ChangedPaths)
		}
		if len(pt.Tags) == 0 {
			return false
		}
		if !matchAny(pt.Tags, ev.Tag) {
			return false
		}
	default:
		// Push to a non-branch, non-tag ref (e.g., refs/notes/*). v1
		// doesn't surface those.
		return false
	}
	return matchPaths(pt.Paths, ev.ChangedPaths)
}

func matchPullRequest(prt *workflow.PullRequestTrigger, ev Event) bool {
	if prt == nil {
		return false
	}
	// Default activity types per GHA when `types:` is omitted.
	types := prt.Types
	if len(types) == 0 {
		types = defaultPRTypes
	}
	if !containsString(types, ev.Action) {
		return false
	}
	// branches: filter applies to the BASE ref (the destination), per
	// GHA docs — that's the branch the PR is targeting.
	if len(prt.Branches) > 0 && !matchAny(prt.Branches, ev.BaseRef) {
		return false
	}
	return matchPaths(prt.Paths, ev.ChangedPaths)
}

func matchSchedule(entries []workflow.ScheduleTrigger, ev Event) bool {
	if len(entries) == 0 {
		return false
	}
	// We require an exact cron-expression match against the entry the
	// sweep fired. The sweep is the source of truth for which cron is
	// firing; we just gate on the workflow having declared that
	// expression. Avoids interpreting cron semantics in two places.
	for _, e := range entries {
		if e.Cron == ev.Cron {
			return true
		}
	}
	return false
}

// matchPaths returns true when paths is empty (no filter) or at least
// one changed path matches the filter list. Empty changed-paths +
// non-empty filter is a miss — we have a filter and nothing to match.
func matchPaths(filter, changed []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, c := range changed {
		if matchAny(filter, c) {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// defaultPRTypes mirrors GHA's default for on.pull_request when no
// types: list is given. Workflow authors who specify types: opt out
// of this default.
var defaultPRTypes = []string{"opened", "synchronize", "reopened"}
