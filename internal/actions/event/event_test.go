// SPDX-License-Identifier: AGPL-3.0-or-later

package event_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/event"
	"github.com/tenseleyFlow/shithub/internal/actions/expr"
)

// TestPush_Shape pins the documented v1 push payload field set:
// ref, before, after, head_commit{message,id,author}. If you're here
// because you added a field and the test failed: update the doc
// (docs/internal/actions-schema.md), bump the v1→v2 marker if this
// is a rename or removal, and update the test in the same PR.
func TestPush_Shape(t *testing.T) {
	t.Parallel()
	p := event.Push("refs/heads/main", "abc", "def", event.HeadCommit{
		Message: "fix: thing", ID: "def", Author: "alice",
	})
	got := keys(p)
	wantTop := []string{"ref", "before", "after", "head_commit"}
	for _, k := range wantTop {
		if !contains(got, k) {
			t.Errorf("missing key %q in push payload (have %v)", k, got)
		}
	}
	hc, ok := p["head_commit"].(map[string]any)
	if !ok {
		t.Fatalf("head_commit not a map: %T", p["head_commit"])
	}
	for _, k := range []string{"message", "id", "author"} {
		if _, ok := hc[k]; !ok {
			t.Errorf("missing head_commit.%s", k)
		}
	}
}

// TestPullRequest_Shape pins the v1 pull_request schema.
func TestPullRequest_Shape(t *testing.T) {
	t.Parallel()
	p := event.PullRequest(
		"opened", 42, "feat: foo",
		event.PRRef{Ref: "feature", SHA: "aaaaaaaa"},
		event.PRRef{Ref: "main", SHA: "bbbbbbbb"},
		"alice",
	)
	for _, k := range []string{"action", "number", "pull_request"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing top-level %s", k)
		}
	}
	pr, ok := p["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("pull_request not a map: %T", p["pull_request"])
	}
	for _, k := range []string{"title", "head", "base", "user"} {
		if _, ok := pr[k]; !ok {
			t.Errorf("missing pull_request.%s", k)
		}
	}
	head := pr["head"].(map[string]any)
	if head["ref"] != "feature" || head["sha"] != "aaaaaaaa" {
		t.Errorf("head ref/sha wrong: %v", head)
	}
	user := pr["user"].(map[string]any)
	if user["login"] != "alice" {
		t.Errorf("user.login wrong: %v", user)
	}
}

// TestSchedule_IsEmptyMap pins the empty-map invariant. Returning nil
// would force callers to nil-check before pgx-encoding; a non-nil
// empty map is the safer default.
func TestSchedule_IsEmptyMap(t *testing.T) {
	t.Parallel()
	p := event.Schedule()
	if p == nil {
		t.Fatal("Schedule() returned nil; expected non-nil empty map")
	}
	if len(p) != 0 {
		t.Errorf("Schedule() should be empty, got %v", p)
	}
}

// TestWorkflowDispatch_Inputs pins the inputs-wrapping shape.
// Authors template ${{ shithub.event.inputs.foo }}, so the inputs key
// must exist as a nested map even when no inputs are provided.
func TestWorkflowDispatch_Inputs(t *testing.T) {
	t.Parallel()
	p := event.WorkflowDispatch(map[string]string{"env": "prod", "tag": "v1.2"})
	inputs, ok := p["inputs"].(map[string]any)
	if !ok {
		t.Fatalf("inputs not a map: %T", p["inputs"])
	}
	if inputs["env"] != "prod" || inputs["tag"] != "v1.2" {
		t.Errorf("inputs wrong: %v", inputs)
	}
}

// TestPush_FlowsThroughEvaluator ties this package to the actual
// expr.evalEventPath consumer. Workflow authors template ${{ ... }}
// against documented field paths; if the constructor lays out a key
// the evaluator can't reach, the contract is broken. Pin both ends.
func TestPush_FlowsThroughEvaluator(t *testing.T) {
	t.Parallel()
	p := event.Push("refs/heads/trunk", "abc", "def", event.HeadCommit{
		Message: "fix: thing", ID: "def", Author: "alice",
	})
	ctx := &expr.Context{
		Shithub:   expr.ShithubContext{Event: p},
		Untrusted: expr.DefaultUntrusted(),
	}
	cases := []struct {
		path string
		want string
	}{
		{`shithub.event.ref`, "refs/heads/trunk"},
		{`shithub.event.head_commit.message`, "fix: thing"},
		{`shithub.event.head_commit.id`, "def"},
		{`shithub.event.head_commit.author`, "alice"},
		{`github.event.head_commit.message`, "fix: thing"}, // alias path
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			toks, err := expr.Lex(tc.path)
			if err != nil {
				t.Fatalf("lex: %v", err)
			}
			ast, err := expr.Parse(toks)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			v, err := expr.Eval(ast, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.S != tc.want {
				t.Errorf("got %q, want %q", v.S, tc.want)
			}
			if !v.Tainted {
				t.Errorf("event-derived value must be tainted")
			}
		})
	}
}

// TestPullRequest_FlowsThroughEvaluator does the same end-to-end pin
// for the pull_request schema, which has the most authoring surface.
func TestPullRequest_FlowsThroughEvaluator(t *testing.T) {
	t.Parallel()
	p := event.PullRequest(
		"opened", 7, "feat: add foo",
		event.PRRef{Ref: "feature", SHA: "feedbeef"},
		event.PRRef{Ref: "main", SHA: "deadbeef"},
		"alice",
	)
	ctx := &expr.Context{
		Shithub:   expr.ShithubContext{Event: p},
		Untrusted: expr.DefaultUntrusted(),
	}
	cases := []struct {
		path string
		want string
	}{
		{`shithub.event.pull_request.title`, "feat: add foo"},
		{`shithub.event.pull_request.head.ref`, "feature"},
		{`shithub.event.pull_request.base.sha`, "deadbeef"},
		{`shithub.event.pull_request.user.login`, "alice"},
		{`shithub.event.action`, "opened"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			toks, err := expr.Lex(tc.path)
			if err != nil {
				t.Fatalf("lex: %v", err)
			}
			ast, err := expr.Parse(toks)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			v, err := expr.Eval(ast, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.S != tc.want {
				t.Errorf("got %q, want %q", v.S, tc.want)
			}
			if !v.Tainted {
				t.Errorf("event-derived value must be tainted")
			}
		})
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
