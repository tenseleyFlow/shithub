// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
)

// Value is the result of evaluating an Expr. It's intentionally
// stringly-typed (no Go interface{} polymorphism) because the types
// we care about — strings + booleans — are both representable as a
// Go string with a Kind tag, and stringly types make the taint flag
// unambiguous to track.
//
// Tainted=true means the value transitively depends on an
// untrusted-source reference (anything in the shithub.event.*
// namespace). The runner's exec layer (S41d) refuses to interpolate
// Tainted values into shell strings.
type Value struct {
	Kind    Kind
	S       string
	B       bool
	Tainted bool
}

// Kind classifies a Value.
type Kind int

const (
	KindString Kind = iota
	KindBool
	KindNull
)

// String renders the Value in the canonical form GHA's evaluator uses:
// strings raw, booleans "true"/"false", null as "". Used by both the
// admin parse subcommand (S41a) and the runner's command rendering
// (S41d).
func (v Value) String() string {
	switch v.Kind {
	case KindBool:
		if v.B {
			return "true"
		}
		return "false"
	case KindNull:
		return ""
	}
	return v.S
}

// Truthy implements GHA's truthiness rules used by `if:` expressions
// and the boolean operators: false, "", null are falsy; anything else
// is truthy. Numeric truthiness (0 falsy) is irrelevant in v1 since
// we don't evaluate arithmetic.
func (v Value) Truthy() bool {
	switch v.Kind {
	case KindBool:
		return v.B
	case KindString:
		return v.S != ""
	}
	return false
}

// Context is the read-only state Eval consults for Ref lookups. The
// caller (S41b trigger pipeline, S41d runner) populates it from the
// triggering domain_event, the workflow's resolved env / secrets /
// vars, and the standard `shithub.*` slots.
//
// Untrusted is the set of namespace prefixes whose values come from
// user-controlled sources (the event payload). Refs landing inside
// any of these get Tainted=true. The default is just "shithub.event"
// — we may extend in v2 if other namespaces grow user-controlled
// fields.
type Context struct {
	Secrets   map[string]string
	Vars      map[string]string
	Env       map[string]string
	EnvTaint  map[string]bool
	Shithub   ShithubContext
	Untrusted map[string]struct{} // namespace prefixes
	JobStatus JobStatus           // for success()/failure()/always()/cancelled()
}

// ShithubContext is the typed `shithub.*` slot. Event is a free-form
// map (the trigger payload, JSON-decoded); the named scalars are
// pre-resolved.
type ShithubContext struct {
	Event map[string]any
	RunID string
	SHA   string
	Ref   string
	Actor string
}

// JobStatus is the rolling status the success()/failure()/always()/
// cancelled() functions consult. Filled by the runner before eval.
type JobStatus struct {
	Failed    bool
	Cancelled bool
}

// DefaultUntrusted returns the standard taint-source allowlist:
// shithub.event.* is user-controlled, everything else is trusted.
func DefaultUntrusted() map[string]struct{} {
	return map[string]struct{}{
		"shithub.event": {},
	}
}

// Eval reduces e against ctx. Errors are precise references back to
// the AST node ("unknown function 'fromJSON'", "secret 'X' not bound",
// etc.). Eval never panics on well-formed input.
func Eval(e Expr, ctx *Context) (Value, error) {
	switch n := e.(type) {
	case LitString:
		return Value{Kind: KindString, S: n.V}, nil
	case LitBool:
		return Value{Kind: KindBool, B: n.V}, nil
	case LitNull:
		return Value{Kind: KindNull}, nil
	case Ref:
		return evalRef(n, ctx)
	case Call:
		return evalCall(n, ctx)
	case Unary:
		x, err := Eval(n.X, ctx)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindBool, B: !x.Truthy(), Tainted: x.Tainted}, nil
	case Binary:
		return evalBinary(n, ctx)
	}
	return Value{}, fmt.Errorf("expr: unknown AST node type")
}

// evalRef walks a dotted path. The first segment selects the
// namespace; subsequent segments index into it. Anything outside the
// allowlist is an error so we don't silently let workflows reach for
// e.g. `runner.os` (which we don't define).
//
// `github.*` is accepted as an alias for `shithub.*` (intentional
// rebrand — see the campaign decision and docs/internal/actions-schema.md).
// We rewrite path[0] from "github" to "shithub" up-front so taint
// computation, dispatch, and error messages all flow through the
// canonical name. github.* refs that don't resolve under shithub.*
// (e.g. `github.event_name`, which we don't expose in v1) error with
// the canonical "unknown shithub field" message — slightly confusing
// for github-flavored authors but actionable.
func evalRef(r Ref, ctx *Context) (Value, error) {
	if len(r.Path) == 0 {
		return Value{}, fmt.Errorf("expr: empty reference")
	}
	path := r.Path
	if path[0] == "github" {
		aliased := make([]string, len(path))
		copy(aliased, path)
		aliased[0] = "shithub"
		path = aliased
	}
	root := path[0]
	tainted := isUntrusted(path, ctx.Untrusted)
	switch root {
	case "secrets":
		if len(path) != 2 {
			return Value{}, fmt.Errorf("expr: secrets.<name> requires exactly one member")
		}
		v, ok := ctx.Secrets[path[1]]
		if !ok {
			return Value{}, fmt.Errorf("expr: secret %q not bound", path[1])
		}
		// Secrets are NEVER tainted — they're operator-controlled.
		// The runner's log scrubber (S41e) replaces their values in
		// log output, but the shell-injection guard cares about
		// untrusted-source taint, not secret-vs-not.
		return Value{Kind: KindString, S: v}, nil
	case "vars":
		if len(path) != 2 {
			return Value{}, fmt.Errorf("expr: vars.<name> requires exactly one member")
		}
		v, ok := ctx.Vars[path[1]]
		if !ok {
			// Missing var resolves to empty string (matches GHA).
			return Value{Kind: KindString, S: ""}, nil
		}
		return Value{Kind: KindString, S: v}, nil
	case "env":
		if len(path) != 2 {
			return Value{}, fmt.Errorf("expr: env.<name> requires exactly one member")
		}
		v, ok := ctx.Env[path[1]]
		if !ok {
			return Value{Kind: KindString, S: ""}, nil
		}
		return Value{Kind: KindString, S: v, Tainted: ctx.EnvTaint[path[1]]}, nil
	case "shithub":
		return evalShithub(path[1:], ctx, tainted)
	}
	return Value{}, fmt.Errorf("expr: unknown namespace %q (allowed: secrets, vars, env, shithub)", root)
}

func evalShithub(rest []string, ctx *Context, tainted bool) (Value, error) {
	if len(rest) == 0 {
		return Value{}, fmt.Errorf("expr: shithub.<...> requires a member")
	}
	switch rest[0] {
	case "run_id":
		return Value{Kind: KindString, S: ctx.Shithub.RunID}, nil
	case "sha":
		return Value{Kind: KindString, S: ctx.Shithub.SHA}, nil
	case "ref":
		return Value{Kind: KindString, S: ctx.Shithub.Ref}, nil
	case "actor":
		return Value{Kind: KindString, S: ctx.Shithub.Actor}, nil
	case "event":
		return evalEventPath(rest[1:], ctx.Shithub.Event, tainted)
	}
	return Value{}, fmt.Errorf("expr: unknown shithub field %q (allowed: run_id, sha, ref, actor, event)", rest[0])
}

// evalEventPath walks into a JSON-decoded map. Missing keys resolve
// to null (GHA convention). Every value here is tainted because the
// payload is user-controlled.
func evalEventPath(path []string, event map[string]any, tainted bool) (Value, error) {
	var cur any = event
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return Value{Kind: KindNull, Tainted: tainted}, nil
		}
		cur = m[key]
	}
	if cur == nil {
		return Value{Kind: KindNull, Tainted: tainted}, nil
	}
	switch v := cur.(type) {
	case string:
		return Value{Kind: KindString, S: v, Tainted: tainted}, nil
	case bool:
		return Value{Kind: KindBool, B: v, Tainted: tainted}, nil
	case float64:
		return Value{Kind: KindString, S: fmt.Sprintf("%v", v), Tainted: tainted}, nil
	}
	// Maps + slices stringify in JSON-like form for if-comparisons.
	return Value{Kind: KindString, S: fmt.Sprintf("%v", cur), Tainted: tainted}, nil
}

// isUntrusted checks if r.Path traces into any prefix in the untrusted
// set. Prefixes are dot-joined like "shithub.event"; a path matches
// when it equals or extends the prefix.
func isUntrusted(path []string, untrusted map[string]struct{}) bool {
	if len(untrusted) == 0 {
		return false
	}
	for i := range path {
		joined := strings.Join(path[:i+1], ".")
		if _, ok := untrusted[joined]; ok {
			return true
		}
	}
	return false
}

// evalCall dispatches the seven allowlisted functions. Any other call
// is an error — this is the closed door the campaign §"Risks" warns
// about ("expression evaluator is a footgun if permissive"). Adding
// a function requires a security note in the commit message.
func evalCall(c Call, ctx *Context) (Value, error) {
	switch c.Name {
	case "contains":
		return strFnArity2(c, ctx, "contains", strings.Contains)
	case "startsWith":
		return strFnArity2(c, ctx, "startsWith", strings.HasPrefix)
	case "endsWith":
		return strFnArity2(c, ctx, "endsWith", strings.HasSuffix)
	case "success":
		if len(c.Args) != 0 {
			return Value{}, fmt.Errorf("expr: success() takes no arguments")
		}
		return Value{Kind: KindBool, B: !ctx.JobStatus.Failed && !ctx.JobStatus.Cancelled}, nil
	case "failure":
		if len(c.Args) != 0 {
			return Value{}, fmt.Errorf("expr: failure() takes no arguments")
		}
		return Value{Kind: KindBool, B: ctx.JobStatus.Failed && !ctx.JobStatus.Cancelled}, nil
	case "always":
		if len(c.Args) != 0 {
			return Value{}, fmt.Errorf("expr: always() takes no arguments")
		}
		return Value{Kind: KindBool, B: true}, nil
	case "cancelled":
		if len(c.Args) != 0 {
			return Value{}, fmt.Errorf("expr: cancelled() takes no arguments")
		}
		return Value{Kind: KindBool, B: ctx.JobStatus.Cancelled}, nil
	}
	return Value{}, fmt.Errorf("expr: unknown function %q (allowed: contains, startsWith, endsWith, success, failure, always, cancelled)", c.Name)
}

func strFnArity2(c Call, ctx *Context, name string, fn func(string, string) bool) (Value, error) {
	if len(c.Args) != 2 {
		return Value{}, fmt.Errorf("expr: %s() takes 2 arguments, got %d", name, len(c.Args))
	}
	a, err := Eval(c.Args[0], ctx)
	if err != nil {
		return Value{}, err
	}
	b, err := Eval(c.Args[1], ctx)
	if err != nil {
		return Value{}, err
	}
	tainted := a.Tainted || b.Tainted
	return Value{Kind: KindBool, B: fn(a.String(), b.String()), Tainted: tainted}, nil
}

func evalBinary(n Binary, ctx *Context) (Value, error) {
	l, err := Eval(n.L, ctx)
	if err != nil {
		return Value{}, err
	}
	switch n.Op {
	case "&&":
		if !l.Truthy() {
			return l, nil // short-circuit — preserves taint
		}
		r, err := Eval(n.R, ctx)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: r.Kind, S: r.S, B: r.B, Tainted: l.Tainted || r.Tainted}, nil
	case "||":
		if l.Truthy() {
			return l, nil
		}
		r, err := Eval(n.R, ctx)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: r.Kind, S: r.S, B: r.B, Tainted: l.Tainted || r.Tainted}, nil
	}
	r, err := Eval(n.R, ctx)
	if err != nil {
		return Value{}, err
	}
	tainted := l.Tainted || r.Tainted
	switch n.Op {
	case "==":
		return Value{Kind: KindBool, B: valuesEqual(l, r), Tainted: tainted}, nil
	case "!=":
		return Value{Kind: KindBool, B: !valuesEqual(l, r), Tainted: tainted}, nil
	}
	return Value{}, fmt.Errorf("expr: unknown binary operator %q", n.Op)
}

func valuesEqual(a, b Value) bool {
	if a.Kind == KindNull || b.Kind == KindNull {
		return a.Kind == b.Kind
	}
	if a.Kind == KindBool && b.Kind == KindBool {
		return a.B == b.B
	}
	return a.String() == b.String()
}
