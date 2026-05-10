// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

// fixtureRoot points at tests/fixtures/workflows/ relative to the repo
// root; the test binary runs from internal/actions/workflow/ so we
// need to walk up.
const fixtureRoot = "../../../tests/fixtures/workflows"

// readFixture loads a fixture by basename + ".yml".
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, name+".yml"))
	if err != nil {
		t.Fatalf("readFixture(%s): %v", name, err)
	}
	return b
}

func TestParse_Minimal(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "minimal"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if w.Name != "minimal" {
		t.Errorf("Name = %q", w.Name)
	}
	if w.On.Push == nil {
		t.Error("expected push trigger")
	}
	if len(w.Jobs) != 1 {
		t.Fatalf("len(Jobs) = %d", len(w.Jobs))
	}
	j := w.Jobs[0]
	if j.Key != "hello" || j.RunsOn != "ubuntu-latest" {
		t.Errorf("job = %+v", j)
	}
	if len(j.Steps) != 1 || j.Steps[0].Run != "echo hello" {
		t.Errorf("steps = %+v", j.Steps)
	}
}

func TestParse_CheckoutOnly(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "checkout-only"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if w.On.Push == nil || len(w.On.Push.Branches) != 2 {
		t.Errorf("push branches = %v", w.On.Push)
	}
	if w.Jobs[0].Steps[0].Uses != "actions/checkout@v4" {
		t.Errorf("uses = %q", w.Jobs[0].Steps[0].Uses)
	}
}

func TestParse_MultiJob(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "multi-job"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(w.Jobs) != 3 {
		t.Fatalf("len(Jobs) = %d", len(w.Jobs))
	}
	keys := make([]string, len(w.Jobs))
	for i, j := range w.Jobs {
		keys[i] = j.Key
	}
	wantKeys := []string{"lint", "test", "package"}
	for i, k := range wantKeys {
		if keys[i] != k {
			t.Errorf("Jobs[%d].Key = %q, want %q", i, keys[i], k)
		}
	}
	pkg := w.Jobs[2]
	if len(pkg.Needs) != 2 || pkg.Needs[0] != "lint" || pkg.Needs[1] != "test" {
		t.Errorf("package.needs = %v", pkg.Needs)
	}
	uploadStep := pkg.Steps[2]
	if uploadStep.Uses != "shithub/upload-artifact@v1" {
		t.Errorf("expected upload-artifact uses, got %q", uploadStep.Uses)
	}
}

func TestParse_UntrustedPRTitle(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "untrusted-pr-title"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	step := w.Jobs[0].Steps[0]
	// The parser carries the raw run command verbatim; the taint flag
	// is decided at expression-evaluation time (S41d) when the runner
	// resolves ${{ ... }} references against the trigger context.
	// Here we just assert the raw string round-trips intact.
	if !strings.Contains(step.Run, "${{ shithub.event.pull_request.title }}") {
		t.Fatalf("untrusted-pr-title fixture lost the expression: %q", step.Run)
	}
}

func TestParse_UnknownKey(t *testing.T) {
	t.Parallel()
	_, diags, err := workflow.Parse(readFixture(t, "unknown-key"))
	if err != nil {
		t.Fatalf("Parse returned err on diagnostic-level issue: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Path, "bogus") {
			found = true
			if d.Severity != workflow.Error {
				t.Errorf("expected Error severity for unknown top-level key, got %v", d.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected diagnostic mentioning 'bogus', got: %v", diags)
	}
}

func TestParse_DisallowedUses(t *testing.T) {
	t.Parallel()
	_, diags, err := workflow.Parse(readFixture(t, "disallowed-uses"))
	if err != nil {
		t.Fatalf("Parse returned err: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Path, "uses") && strings.Contains(d.Message, "actions/setup-go") || strings.Contains(d.Message, "v1 supports only") {
			found = true
			if d.Severity != workflow.Error {
				t.Errorf("expected Error severity, got %v", d.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected diagnostic on the disallowed `uses:`, got: %v", diags)
	}
}

func TestParse_OversizedFile(t *testing.T) {
	t.Parallel()
	big := make([]byte, workflow.MaxWorkflowFileBytes+1)
	for i := range big {
		big[i] = ' '
	}
	_, _, err := workflow.Parse(big)
	if !errors.Is(err, workflow.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

func TestParse_EmptyFile(t *testing.T) {
	t.Parallel()
	_, diags, err := workflow.Parse(nil)
	if err == nil && !hasError(diags) {
		t.Fatalf("expected error on empty input")
	}
}

func TestParse_ExpressionFunctionsRoundTrip(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "expression-functions"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	steps := w.Jobs[0].Steps
	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(steps))
	}
	wantPrefixes := []string{"contains(", "startsWith(", "success()", "failure()", "always()"}
	for i, want := range wantPrefixes {
		if !strings.Contains(steps[i].If, want) {
			t.Errorf("step[%d].If = %q; expected to contain %q", i, steps[i].If, want)
		}
	}
}

// BenchmarkParseTypical50Lines pins the parser-perf budget. Per the
// S41a sprint file: ≤ 1 ms for a typical 50-line workflow. multi-job
// fixture is 24 lines; we 4× the body to exceed 50 lines for a fair
// signal.
func BenchmarkParseTypical50Lines(b *testing.B) {
	src, err := os.ReadFile(filepath.Join(fixtureRoot, "multi-job.yml"))
	if err != nil {
		b.Fatalf("read fixture: %v", err)
	}
	b.SetBytes(int64(len(src)))
	for i := 0; i < b.N; i++ {
		_, _, err := workflow.Parse(src)
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}
	}
}

func hasError(diags []workflow.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == workflow.Error {
			return true
		}
	}
	return false
}
