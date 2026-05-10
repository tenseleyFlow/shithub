// SPDX-License-Identifier: AGPL-3.0-or-later

package expr_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
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

// TestEval_GithubAliasResolves exercises the documented `${{ github.* }}`
// → `${{ shithub.* }}` rebrand alias. Workflow authors copy-pasting GHA
// workflows expect the github namespace to keep working; the evaluator
// rewrites at the namespace boundary and routes through shithub semantics.
// Audit S41a-H1 found this was dead code; this test pins it live.
func TestEval_GithubAliasResolves(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	cases := []struct {
		src  string
		want string
	}{
		{`github.run_id`, "42"},
		{`github.sha`, "deadbeef"},
		{`github.actor`, "alice"},
		{`github.ref`, "refs/heads/trunk"},
		{`github.event.pull_request.title`, "feat: add foo"},
		{`github.event.head_commit.message`, "WIP: testing"},
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

// TestEval_GithubAliasIsTainted asserts the rebrand alias preserves the
// load-bearing taint flag: github.event.* must taint exactly like
// shithub.event.*. If the alias rewrite happens after isUntrusted runs,
// taint quietly disappears and S41d's injection guard misses untrusted
// PR-title input. Pin it.
func TestEval_GithubAliasIsTainted(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	v, err := evalString(t, `github.event.pull_request.title`, ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !v.Tainted {
		t.Fatal("github.event.* must be tainted (load-bearing for S41d injection prevention)")
	}
}

// TestEval_GithubAliasNonEventNotTainted is the inverse pin: github.run_id
// (and friends) ride the same alias path but must NOT be tainted because
// they don't derive from the user-controlled event payload.
func TestEval_GithubAliasNonEventNotTainted(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	for _, src := range []string{`github.run_id`, `github.sha`, `github.actor`, `github.ref`} {
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

// TestEval_GithubUnknownFieldErrors confirms the alias is *narrow*: only
// the shithub.{run_id,sha,ref,actor,event} subset routes through. github
// fields we don't expose (event_name, repository, run_number, etc.) get
// the canonical shithub error message — slightly confusing for a github-
// flavored author but actionable.
func TestEval_GithubUnknownFieldErrors(t *testing.T) {
	t.Parallel()
	ctx := defaultContext()
	for _, src := range []string{`github.event_name`, `github.repository`, `github.run_number`} {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			_, err := evalString(t, src, ctx)
			if err == nil {
				t.Fatalf("expected eval error for unsupported github.* field %s", src)
			}
			if !strings.Contains(err.Error(), "unknown shithub field") {
				t.Errorf("expected 'unknown shithub field' (canonical), got: %v", err)
			}
		})
	}
}

// TestEval_GithubAliasFixtureEndToEnd lifts the actual github-alias.yml
// fixture, parses the workflow, extracts the `${{ … }}` body from the
// step's run command, and evaluates it through the alias path. This is
// the end-to-end pin that would have caught S41a-H1: the fixture
// existed but no test exercised it through eval.
func TestEval_GithubAliasFixtureEndToEnd(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("../../../tests/fixtures/workflows", "github-alias.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	w, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	// Step 0's run command is: echo "${{ github.event.head_commit.message }}"
	run := w.Jobs[0].Steps[0].Run
	body, ok := extractFirstExpression(run)
	if !ok {
		t.Fatalf("expected ${{ ... }} expression in run command, got %q", run)
	}
	v, err := evalString(t, body, &expr.Context{
		Shithub: expr.ShithubContext{
			Event: map[string]any{
				"head_commit": map[string]any{
					"message": "WIP: from fixture",
				},
			},
		},
		Untrusted: expr.DefaultUntrusted(),
	})
	if err != nil {
		t.Fatalf("eval %q: %v", body, err)
	}
	if v.S != "WIP: from fixture" {
		t.Errorf("got %q, want %q", v.S, "WIP: from fixture")
	}
	if !v.Tainted {
		t.Error("github.event.head_commit.message must be tainted (S41d injection guard)")
	}
}

// extractFirstExpression pulls the body of the first `${{ … }}` block
// out of s. Tiny helper used by the fixture round-trip test; the real
// runner-side templating in S41d will be more sophisticated.
func extractFirstExpression(s string) (string, bool) {
	start := strings.Index(s, "${{")
	if start < 0 {
		return "", false
	}
	end := strings.Index(s[start:], "}}")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(s[start+3 : start+end]), true
}

// TestLex_UnicodeIdentifier pins rune-aware identifier lexing. The
// pre-M1 byte-level lexer would fail mid-byte on multi-byte UTF-8
// characters with a confusing "unexpected character" error pointing
// at a continuation byte. After M1, identifiers can contain any
// unicode.IsLetter rune. This is a quality fix, not a security one
// — the byte-level lexer failed closed; the rune-aware one accepts
// more identifiers but the namespace allowlist still rejects them
// at eval time.
func TestLex_UnicodeIdentifier(t *testing.T) {
	t.Parallel()
	cases := []string{
		"αlpha",      // Greek letter
		"über",       // Latin-1 supplement
		"日本語",        // CJK
		"snake_α",    // mixed ASCII + non-ASCII
		"_underline", // leading underscore
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			toks, err := expr.Lex(src)
			if err != nil {
				t.Fatalf("lex %q: %v", src, err)
			}
			if len(toks) != 2 || toks[0].Kind != expr.TokIdent {
				t.Fatalf("expected single Ident token + EOF, got %+v", toks)
			}
			if toks[0].Value != src {
				t.Errorf("ident value: got %q, want %q", toks[0].Value, src)
			}
		})
	}
}

// TestLex_InvalidUTF8 surfaces a clean error message when src isn't
// valid UTF-8 — pre-M1 the lexer would feed RuneError bytes through
// the byte-level loop and produce a misleading offset.
func TestLex_InvalidUTF8(t *testing.T) {
	t.Parallel()
	_, err := expr.Lex(string([]byte{'a', 0x80, 'b'}))
	if err == nil {
		t.Fatal("expected error on invalid UTF-8")
	}
	if !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Errorf("expected 'invalid UTF-8' error, got: %v", err)
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
