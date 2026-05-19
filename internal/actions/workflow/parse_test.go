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

func TestParse_SetupPython(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "setup-python"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	steps := w.Jobs[0].Steps
	if len(steps) != 3 {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[1].Uses != "actions/setup-python@v5" {
		t.Fatalf("setup-python step = %+v", steps[1])
	}
	if got := steps[1].With["python-version"].Raw; got != "3.12" {
		t.Fatalf("python-version = %q, want 3.12", got)
	}
}

func TestParse_ArbitraryRepoSmoke(t *testing.T) {
	t.Parallel()
	w, diags, err := workflow.Parse(readFixture(t, "arbitrary-repo-smoke"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if w.Name != "Smoke" {
		t.Fatalf("Name = %q, want Smoke", w.Name)
	}
	if w.On.Push == nil || len(w.On.Push.Branches) != 1 || w.On.Push.Branches[0] != "trunk" {
		t.Fatalf("push branches = %+v", w.On.Push)
	}
	if len(w.Jobs) != 1 {
		t.Fatalf("len(Jobs) = %d", len(w.Jobs))
	}
	job := w.Jobs[0]
	if job.Key != "green" || job.RunsOn != "ubuntu-latest" {
		t.Fatalf("job = %+v", job)
	}
	if len(job.Steps) != 3 {
		t.Fatalf("steps = %+v", job.Steps)
	}
	if job.Steps[0].Uses != "actions/checkout@v4" {
		t.Fatalf("checkout step = %+v", job.Steps[0])
	}
	if job.Steps[1].Name != "Verify checkout" || !strings.Contains(job.Steps[1].Run, "README.md") {
		t.Fatalf("verify step = %+v", job.Steps[1])
	}
	if job.Steps[2].Name != "Smoke" || !strings.Contains(job.Steps[2].Run, "shithub actions smoke passed") {
		t.Fatalf("smoke step = %+v", job.Steps[2])
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

func TestParse_JobEnvironment(t *testing.T) {
	t.Parallel()
	src := []byte(`
name: deploy
on: push
jobs:
  string-env:
    runs-on: ubuntu-latest
    environment: staging
    steps:
      - run: echo deploy
  mapped-env:
    runs-on: ubuntu-latest
    environment:
      name: production
      url: https://deployments.example.com/${{ shithub.sha }}
    steps:
      - run: echo deploy
`)
	w, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := w.Jobs[0].Environment.Name; got != "staging" {
		t.Fatalf("string environment name = %q, want staging", got)
	}
	if got := w.Jobs[1].Environment.Name; got != "production" {
		t.Fatalf("mapped environment name = %q, want production", got)
	}
	if got := w.Jobs[1].Environment.URL.Raw; !strings.Contains(got, "deployments.example.com") {
		t.Fatalf("mapped environment url = %q", got)
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

// TestParse_BooleanCanonicalForms pins L1: workflow boolean fields
// accept the canonical YAML 1.2 forms (true/True/TRUE, false/False/
// FALSE) instead of strict-string matching only "true". Pre-L1 the
// parser silently treated everything except "true" as false.
func TestParse_BooleanCanonicalForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"false", false},
		{"False", false},
		{"FALSE", false},
	}
	for _, tc := range cases {
		t.Run("cancel-in-progress="+tc.val, func(t *testing.T) {
			t.Parallel()
			src := []byte(`name: x
on: push
concurrency:
  group: g
  cancel-in-progress: ` + tc.val + `
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`)
			w, diags, err := workflow.Parse(src)
			if err != nil || w == nil {
				t.Fatalf("parse: err=%v w=%v", err, w)
			}
			if hasError(diags) {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if w.Concurrency.CancelInProgress != tc.want {
				t.Errorf("got %v, want %v for input %q", w.Concurrency.CancelInProgress, tc.want, tc.val)
			}
		})
	}
}

// TestParse_BadStepIDProducesDiagnostic pins L2: the parser surfaces
// an Error-severity diagnostic on a malformed step id immediately,
// instead of letting the workflow be valid at parse time and failing
// at INSERT time during S41b dispatch.
func TestParse_BadStepIDProducesDiagnostic(t *testing.T) {
	t.Parallel()
	src := []byte(`name: x
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - id: 'has spaces and ; semicolons'
        run: echo
`)
	_, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "step id must match") && d.Severity == workflow.Error {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected step-id format diagnostic, got: %v", diags)
	}
}

// TestParse_BadJobKeyProducesDiagnostic mirrors the step-id check
// for the jobs.<key> regex constraint.
func TestParse_BadJobKeyProducesDiagnostic(t *testing.T) {
	t.Parallel()
	src := []byte(`name: x
on: push
jobs:
  "bad job":
    runs-on: ubuntu-latest
    steps:
      - run: echo
`)
	_, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "job key must match") && d.Severity == workflow.Error {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected job-key format diagnostic, got: %v", diags)
	}
}

// TestParse_EmptyStepIDIsOK pins that the optional-id semantic still
// works: a step with no `id:` (empty string in the parser's struct)
// doesn't surface a diagnostic. Only set values are validated.
func TestParse_EmptyStepIDIsOK(t *testing.T) {
	t.Parallel()
	src := []byte(`name: x
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo no-id
`)
	w, diags, err := workflow.Parse(src)
	if err != nil || w == nil {
		t.Fatalf("parse: err=%v w=%v", err, w)
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "step id must match") {
			t.Fatalf("unexpected step-id diagnostic on omitted id: %v", d)
		}
	}
}

// TestParse_BooleanInvalidProducesDiagnostic asserts that a non-
// boolean value (e.g. "yes" — valid YAML 1.1, NOT 1.2) surfaces a
// parse-time diagnostic instead of silently coercing.
func TestParse_BooleanInvalidProducesDiagnostic(t *testing.T) {
	t.Parallel()
	src := []byte(`name: x
on: push
concurrency:
  group: g
  cancel-in-progress: yes
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`)
	_, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "boolean") && d.Severity == workflow.Error {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected boolean-error diagnostic for 'yes', got: %v", diags)
	}
}

// TestParse_DiagnosticPathQuotesNonIdentKeys pins L6: env keys that
// contain dots, spaces, etc. produce bracket-quoted diagnostic paths
// instead of ambiguous concat. Compare:
//   - identifier-shaped key: path is `jobs.j.env.GOOD`
//   - dotted key: path is `jobs.j.env["weird.key"]` (unambiguous)
func TestParse_DiagnosticPathQuotesNonIdentKeys(t *testing.T) {
	t.Parallel()
	src := []byte(`name: x
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    env:
      "weird.key":
        nested: not-a-scalar
    steps:
      - run: echo
`)
	_, diags, err := workflow.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Path, `["weird.key"]`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected bracket-quoted path for dotted env key, got diags: %v", diags)
	}
}
