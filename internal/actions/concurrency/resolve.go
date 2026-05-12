// SPDX-License-Identifier: AGPL-3.0-or-later

package concurrency

import (
	"fmt"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
)

// EvalContext is the limited trigger-time expression context for a
// concurrency.group value. It deliberately excludes secrets.
type EvalContext struct {
	EventPayload map[string]any
	HeadSHA      string
	HeadRef      string
}

// ResolveGroup evaluates `${{ ... }}` fragments in a concurrency group value.
// Literal text outside expressions is preserved.
func ResolveGroup(raw string, in EvalContext) (string, error) {
	ctx := expr.Context{
		Shithub: expr.ShithubContext{
			Event: in.EventPayload,
			SHA:   in.HeadSHA,
			Ref:   in.HeadRef,
		},
		Untrusted: expr.DefaultUntrusted(),
	}
	var out strings.Builder
	if err := walkExpressions(raw, func(literal, body string) error {
		if body == "" {
			out.WriteString(literal)
			return nil
		}
		out.WriteString(literal)
		v, err := eval(body, &ctx)
		if err != nil {
			return err
		}
		out.WriteString(v.String())
		return nil
	}); err != nil {
		return "", fmt.Errorf("actions concurrency: resolve group: %w", err)
	}
	return validateGroup(out.String())
}

func eval(body string, ctx *expr.Context) (expr.Value, error) {
	toks, err := expr.Lex(strings.TrimSpace(body))
	if err != nil {
		return expr.Value{}, err
	}
	ast, err := expr.Parse(toks)
	if err != nil {
		return expr.Value{}, err
	}
	return expr.Eval(ast, ctx)
}

func walkExpressions(raw string, fn func(literal, body string) error) error {
	for {
		start := strings.Index(raw, "${{")
		if start < 0 {
			return fn(raw, "")
		}
		end := strings.Index(raw[start+3:], "}}")
		if end < 0 {
			return fmt.Errorf("render expression: missing closing }}")
		}
		end += start + 3
		if err := fn(raw[:start], raw[start+3:end]); err != nil {
			return err
		}
		raw = raw[end+2:]
	}
}
