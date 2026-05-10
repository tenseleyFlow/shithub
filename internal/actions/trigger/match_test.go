// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

// wf is a tiny constructor that returns a Workflow with the supplied
// TriggerSet — most match tests don't care about other fields.
func wf(on workflow.TriggerSet) *workflow.Workflow {
	return &workflow.Workflow{On: on}
}

func TestMatch_PushBranches(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{
		Push: &workflow.PushTrigger{Branches: []string{"main", "release/**"}},
	})
	cases := []struct {
		name string
		ev   trigger.Event
		want bool
	}{
		{"main hits", trigger.Event{Kind: trigger.EventPush, Branch: "main"}, true},
		{"release/v1 hits", trigger.Event{Kind: trigger.EventPush, Branch: "release/v1"}, true},
		{"release/v1/hotfix hits", trigger.Event{Kind: trigger.EventPush, Branch: "release/v1/hotfix"}, true},
		{"feature misses", trigger.Event{Kind: trigger.EventPush, Branch: "feature/foo"}, false},
		{"tag push doesn't match branches filter", trigger.Event{Kind: trigger.EventPush, Tag: "v1.0.0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := trigger.Match(w, tc.ev)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMatch_PushTags(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{
		Push: &workflow.PushTrigger{Tags: []string{"v*", "!v0.*"}},
	})
	cases := []struct {
		name string
		ev   trigger.Event
		want bool
	}{
		{"v1.0.0 hits", trigger.Event{Kind: trigger.EventPush, Tag: "v1.0.0"}, true},
		{"v0.5 excluded", trigger.Event{Kind: trigger.EventPush, Tag: "v0.5"}, false},
		{"branch doesn't match tags filter", trigger.Event{Kind: trigger.EventPush, Branch: "main"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := trigger.Match(w, tc.ev)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMatch_PushPaths(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{
		Push: &workflow.PushTrigger{Paths: []string{"src/**", "**/*.md"}},
	})
	ev := func(branch string, paths ...string) trigger.Event {
		return trigger.Event{Kind: trigger.EventPush, Branch: branch, ChangedPaths: paths}
	}
	cases := []struct {
		name string
		ev   trigger.Event
		want bool
	}{
		{"src hit", ev("main", "src/main.go"), true},
		{"deep src hit", ev("main", "src/pkg/sub/x.go"), true},
		{"top-level md", ev("main", "README.md"), true},
		{"vendored docs", ev("main", "third_party/lib/README.md"), true},
		{"dist only — miss", ev("main", "dist/out.tar"), false},
		{"empty changed list — miss", ev("main"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := trigger.Match(w, tc.ev)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMatch_PullRequest_DefaultTypes(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{PullRequest: &workflow.PullRequestTrigger{}})
	cases := []struct {
		action string
		want   bool
	}{
		{"opened", true},
		{"synchronize", true},
		{"reopened", true},
		{"closed", false},
		{"labeled", false},
		{"edited", false},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			got := trigger.Match(w, trigger.Event{
				Kind: trigger.EventPullRequest, Action: tc.action, BaseRef: "main",
			})
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMatch_PullRequest_CustomTypesAndBranches(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{
		PullRequest: &workflow.PullRequestTrigger{
			Types:    []string{"closed", "labeled"},
			Branches: []string{"main"},
		},
	})
	hit := trigger.Event{Kind: trigger.EventPullRequest, Action: "closed", BaseRef: "main"}
	if !trigger.Match(w, hit) {
		t.Error("custom types should accept 'closed' targeting main")
	}
	missAction := trigger.Event{Kind: trigger.EventPullRequest, Action: "opened", BaseRef: "main"}
	if trigger.Match(w, missAction) {
		t.Error("'opened' should miss when types: doesn't list it")
	}
	missBase := trigger.Event{Kind: trigger.EventPullRequest, Action: "closed", BaseRef: "release"}
	if trigger.Match(w, missBase) {
		t.Error("base != main should miss with branches: ['main']")
	}
}

func TestMatch_Schedule(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{
		Schedule: []workflow.ScheduleTrigger{
			{Cron: "*/5 * * * *"},
			{Cron: "0 6 * * MON"},
		},
	})
	if !trigger.Match(w, trigger.Event{Kind: trigger.EventSchedule, Cron: "*/5 * * * *"}) {
		t.Error("declared cron must match")
	}
	if trigger.Match(w, trigger.Event{Kind: trigger.EventSchedule, Cron: "0 0 * * *"}) {
		t.Error("undeclared cron must NOT match")
	}
}

func TestMatch_WorkflowDispatch(t *testing.T) {
	t.Parallel()
	with := wf(workflow.TriggerSet{WorkflowDispatch: &workflow.WorkflowDispatchTrigger{}})
	without := wf(workflow.TriggerSet{Push: &workflow.PushTrigger{}})
	if !trigger.Match(with, trigger.Event{Kind: trigger.EventWorkflowDispatch}) {
		t.Error("workflow_dispatch trigger must match when declared")
	}
	if trigger.Match(without, trigger.Event{Kind: trigger.EventWorkflowDispatch}) {
		t.Error("workflow without on.workflow_dispatch must NOT match")
	}
}

func TestMatch_KindNotDeclared(t *testing.T) {
	t.Parallel()
	// Workflow only listens for push; PR event must miss.
	w := wf(workflow.TriggerSet{Push: &workflow.PushTrigger{}})
	if trigger.Match(w, trigger.Event{Kind: trigger.EventPullRequest, Action: "opened", BaseRef: "main"}) {
		t.Error("PR event must NOT match a push-only workflow")
	}
}

func TestMatch_NilWorkflow(t *testing.T) {
	t.Parallel()
	if trigger.Match(nil, trigger.Event{Kind: trigger.EventPush, Branch: "main"}) {
		t.Error("nil workflow must never match (defensive zero)")
	}
}

func TestMatch_UnknownKind(t *testing.T) {
	t.Parallel()
	w := wf(workflow.TriggerSet{Push: &workflow.PushTrigger{}})
	if trigger.Match(w, trigger.Event{Kind: trigger.EventKind("release")}) {
		t.Error("unknown kind must NOT match (strict allowlist)")
	}
}
