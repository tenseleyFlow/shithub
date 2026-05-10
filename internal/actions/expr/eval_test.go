// SPDX-License-Identifier: AGPL-3.0-or-later

package expr_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
)

// evalString is the test helper that lex + parse + eval in one shot.
func evalString(t *testing.T, src string, ctx *expr.Context) (expr.Value, error) {
	t.Helper()
	toks, err := expr.Lex(src)
	if err != nil {
		return expr.Value{}, err
	}
	ast, err := expr.Parse(toks)
	if err != nil {
		return expr.Value{}, err
	}
	return expr.Eval(ast, ctx)
}

func defaultContext() *expr.Context {
	return &expr.Context{
		Secrets: map[string]string{"MY_SECRET": "shh"},
		Vars:    map[string]string{"REGION": "us-east-1"},
		Env:     map[string]string{"GREETING": "hello"},
		Shithub: expr.ShithubContext{
			RunID: "42",
			SHA:   "deadbeef",
			Ref:   "refs/heads/trunk",
			Actor: "alice",
			Event: map[string]any{
				"pull_request": map[string]any{
					"title": "feat: add foo",
					"head": map[string]any{
						"ref": "feat/foo",
					},
				},
				"head_commit": map[string]any{
					"message": "WIP: testing",
				},
			},
		},
		Untrusted: expr.DefaultUntrusted(),
	}
}

func TestEval_LiteralAndRefs(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	cases := []struct {
		src  string
		want string
	}{
		{`'hello'`, "hello"},
		{`secrets.MY_SECRET`, "shh"},
		{`vars.REGION`, "us-east-1"},
		{`env.GREETING`, "hello"},
		{`shithub.run_id`, "42"},
		{`shithub.sha`, "deadbeef"},
		{`shithub.actor`, "alice"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			v, err := evalString(t, tc.src, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.S != tc.want {
				t.Errorf("got %q, want %q", v.S, tc.want)
			}
		})
	}
}

func TestEval_TaintFromEvent(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	v, err := evalString(t, `shithub.event.pull_request.title`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v.S != "feat: add foo" {
		t.Errorf("got %q", v.S)
	}
	if !v.Tainted {
		t.Fatal("expected Tainted=true on shithub.event.* reference (load-bearing for S41d injection prevention)")
	}
}

func TestEval_TaintNotFromTrustedSources(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	for _, src := range []string{
		`secrets.MY_SECRET`,
		`vars.REGION`,
		`env.GREETING`,
		`shithub.run_id`,
		`shithub.sha`,
		`shithub.actor`,
	} {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			v, err := evalString(t, src, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.Tainted {
				t.Fatalf("expected Tainted=false on %s; got Tainted=true (would falsely trip S41d guard)", src)
			}
		})
	}
}

func TestEval_TaintPropagatesThroughBinary(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	// 'WIP' compared to a tainted value → result tainted.
	v, err := evalString(t, `shithub.event.pull_request.title == 'WIP'`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !v.Tainted {
		t.Fatal("equality with a tainted operand must be tainted")
	}
}

func TestEval_TaintPropagatesThroughFunction(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	v, err := evalString(t, `contains(shithub.event.head_commit.message, 'WIP')`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !v.B {
		t.Errorf("expected contains() to return true; got false")
	}
	if !v.Tainted {
		t.Fatal("contains() with a tainted operand must be tainted")
	}
}

func TestEval_AllowedFunctions(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	cases := []struct {
		src  string
		want bool
	}{
		{`contains('hello world', 'world')`, true},
		{`contains('hello', 'WORLD')`, false},
		{`startsWith('refs/heads/release/v1', 'refs/heads/release/')`, true},
		{`endsWith('foo.tar.gz', '.gz')`, true},
		{`success()`, true}, // JobStatus zero-value: not failed, not cancelled
		{`failure()`, false},
		{`always()`, true},
		{`cancelled()`, false},
		{`!success()`, false},
		{`true && false`, false},
		{`true || false`, true},
		{`'a' == 'a'`, true},
		{`'a' != 'b'`, true},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			v, err := evalString(t, tc.src, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.B != tc.want {
				t.Errorf("got %v, want %v", v.B, tc.want)
			}
		})
	}
}

func TestEval_DisallowedFunctionFails(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	cases := []string{
		`fromJSON('{}')`,
		`hashFiles('**/*.go')`,
		`toJSON('foo')`,
		`format('hello {0}', 'world')`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			_, err := evalString(t, src, ctx)
			if err == nil {
				t.Fatal("expected eval error for disallowed function")
			}
			if !strings.Contains(err.Error(), "unknown function") {
				t.Errorf("expected 'unknown function' error, got: %v", err)
			}
		})
	}
}

func TestEval_DisallowedNamespaceFails(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	// Each of these would let workflows reach into a namespace we
	// don't want to support in v1. Some fail at lex (when the
	// identifier shape isn't lex-valid Go-ish, e.g. `go-version`),
	// some at eval. Both are correct rejections — neither lets
	// the workflow author smuggle a value through.
	cases := []string{
		`runner.os`,
		`steps.foo.outputs.bar`,
		`needs.lint.result`,
		`matrix.versions`,
		`inputs.foo`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			_, err := evalString(t, src, ctx)
			if err == nil {
				t.Fatal("expected eval error for disallowed namespace")
			}
			if !strings.Contains(err.Error(), "unknown namespace") {
				t.Errorf("expected 'unknown namespace' error, got: %v", err)
			}
		})
	}
}

func TestEval_MissingSecretIsError(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	_, err := evalString(t, `secrets.NOT_BOUND`, ctx)
	if err == nil {
		t.Fatal("expected error for unbound secret")
	}
	if !strings.Contains(err.Error(), "not bound") {
		t.Errorf("expected 'not bound', got: %v", err)
	}
}

func TestEval_MissingVarIsEmpty(t *testing.T) {
	t.Parallel()
	// vars (and env) match GHA semantics: missing → empty string, not error.
	ctx := defaultContext()
	v, err := evalString(t, `vars.MISSING`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v.S != "" {
		t.Errorf("got %q", v.S)
	}
}

func TestEval_MissingEventPathReturnsNull(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	v, err := evalString(t, `shithub.event.deeply.nested.missing`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v.Kind != expr.KindNull {
		t.Errorf("got Kind=%v, want KindNull", v.Kind)
	}
	if !v.Tainted {
		t.Errorf("missing-key result from event.* must still be tainted (it derives from the event payload)")
	}
}

func TestEval_StringEscapeQuote(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	v, err := evalString(t, `'it''s ok'`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v.S != "it's ok" {
		t.Errorf("got %q", v.S)
	}
}

func TestEval_JobStatusFunctions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		failed, cancelled bool
		src               string
		want              bool
	}{
		{false, false, `success()`, true},
		{true, false, `success()`, false},
		{true, false, `failure()`, true},
		{false, true, `cancelled()`, true},
		{false, true, `failure()`, false}, // cancelled overrides failure
		{true, true, `failure()`, false},
		{true, true, `always()`, true},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			ctx := defaultContext()
			ctx.JobStatus = expr.JobStatus{Failed: tc.failed, Cancelled: tc.cancelled}
			v, err := evalString(t, tc.src, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if v.B != tc.want {
				t.Errorf("failed=%v cancelled=%v %s = %v, want %v",
					tc.failed, tc.cancelled, tc.src, v.B, tc.want)
			}
		})
	}
}
