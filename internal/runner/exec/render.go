// SPDX-License-Identifier: AGPL-3.0-or-later

// Package exec renders workflow expressions into the shell/env surface used
// by runner engines. It is deliberately separate from the Docker engine so
// every future engine consumes the same taint boundary.
package exec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
)

const defaultBindingPrefix = "SHITHUB_INPUT_"

type Bindings struct {
	prefix string
	next   int
	env    map[string]string
}

func NewBindings(prefix string) *Bindings {
	if strings.TrimSpace(prefix) == "" {
		prefix = defaultBindingPrefix
	}
	return &Bindings{prefix: prefix, env: map[string]string{}}
}

func (b *Bindings) Add(value string) string {
	name := b.prefix + strconv.Itoa(b.next)
	b.next++
	b.env[name] = value
	return name
}

func (b *Bindings) Env() map[string]string {
	out := make(map[string]string, len(b.env))
	for k, v := range b.env {
		out[k] = v
	}
	return out
}

type ResolvedText struct {
	Text    string
	Tainted bool
}

type RenderedStep struct {
	Run      string
	Env      map[string]string
	EnvTaint map[string]bool
}

type StepInput struct {
	Run     string
	JobEnv  map[string]string
	StepEnv map[string]string
	Context expr.Context
}

func RenderStep(in StepInput) (RenderedStep, error) {
	ctx := cloneContext(&in.Context)
	bindings := NewBindings("")

	env, taint, err := resolveEnv(in.JobEnv, &ctx)
	if err != nil {
		return RenderedStep{}, fmt.Errorf("render job env: %w", err)
	}
	mergeContextEnv(&ctx, env, taint)

	stepEnv, stepTaint, err := resolveEnv(in.StepEnv, &ctx)
	if err != nil {
		return RenderedStep{}, fmt.Errorf("render step env: %w", err)
	}
	for k, v := range stepEnv {
		env[k] = v
		if stepTaint[k] {
			taint[k] = true
		} else {
			delete(taint, k)
		}
	}
	mergeContextEnv(&ctx, stepEnv, stepTaint)

	run, err := RenderShell(in.Run, &ctx, bindings)
	if err != nil {
		return RenderedStep{}, err
	}
	for k, v := range bindings.Env() {
		env[k] = v
	}
	return RenderedStep{Run: run, Env: env, EnvTaint: taint}, nil
}

func RenderShell(raw string, ctx *expr.Context, bindings *Bindings) (string, error) {
	if bindings == nil {
		bindings = NewBindings("")
	}
	var out strings.Builder
	if err := walkExpressions(raw, func(literal, body string) error {
		if body == "" {
			out.WriteString(literal)
			return nil
		}
		out.WriteString(literal)
		v, err := eval(body, ctx)
		if err != nil {
			return err
		}
		if v.Tainted {
			out.WriteString("${")
			out.WriteString(bindings.Add(v.String()))
			out.WriteString("}")
			return nil
		}
		out.WriteString(v.String())
		return nil
	}); err != nil {
		return "", err
	}
	return out.String(), nil
}

func ResolveText(raw string, ctx *expr.Context) (ResolvedText, error) {
	var out strings.Builder
	tainted := false
	if err := walkExpressions(raw, func(literal, body string) error {
		if body == "" {
			out.WriteString(literal)
			return nil
		}
		out.WriteString(literal)
		v, err := eval(body, ctx)
		if err != nil {
			return err
		}
		if v.Tainted {
			tainted = true
		}
		out.WriteString(v.String())
		return nil
	}); err != nil {
		return ResolvedText{}, err
	}
	return ResolvedText{Text: out.String(), Tainted: tainted}, nil
}

func resolveEnv(raw map[string]string, ctx *expr.Context) (map[string]string, map[string]bool, error) {
	out := make(map[string]string, len(raw))
	taint := make(map[string]bool, len(raw))
	for _, key := range sortedKeys(raw) {
		if strings.HasPrefix(key, defaultBindingPrefix) {
			return nil, nil, fmt.Errorf("%s uses reserved runner input prefix %s", key, defaultBindingPrefix)
		}
		v, err := ResolveText(raw[key], ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = v.Text
		if v.Tainted {
			taint[key] = true
		}
	}
	return out, taint, nil
}

func eval(body string, ctx *expr.Context) (expr.Value, error) {
	if ctx == nil {
		c := expr.Context{Untrusted: expr.DefaultUntrusted()}
		ctx = &c
	}
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

func cloneContext(ctx *expr.Context) expr.Context {
	if ctx == nil {
		return expr.Context{Untrusted: expr.DefaultUntrusted()}
	}
	out := *ctx
	out.Secrets = cloneStringMap(ctx.Secrets)
	out.Vars = cloneStringMap(ctx.Vars)
	out.Env = cloneStringMap(ctx.Env)
	out.EnvTaint = cloneBoolMap(ctx.EnvTaint)
	out.Untrusted = cloneSet(ctx.Untrusted)
	out.Shithub.Event = cloneAnyMap(ctx.Shithub.Event)
	return out
}

func mergeContextEnv(ctx *expr.Context, env map[string]string, taint map[string]bool) {
	if ctx.Env == nil {
		ctx.Env = map[string]string{}
	}
	if ctx.EnvTaint == nil {
		ctx.EnvTaint = map[string]bool{}
	}
	for k, v := range env {
		ctx.Env[k] = v
		if taint[k] {
			ctx.EnvTaint[k] = true
		} else {
			delete(ctx.EnvTaint, k)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return expr.DefaultUntrusted()
	}
	out := make(map[string]struct{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
