// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workflow parses .shithub/workflows/*.yml files into the typed
// Workflow tree S41b's trigger pipeline + S41c's secret resolver +
// S41d's runner all consume.
//
// The parser is intentionally strict: unknown top-level keys, unknown
// step keys, unknown `uses:` references, and `${{ … }}` expressions
// outside the allowlist all produce diagnostics. Workflow authors
// catch their mistakes immediately instead of silently shipping a
// workflow that does nothing.
//
// Every value that can carry user-controlled text (event payload
// fields like PR title, branch name, etc.) is tagged with
// Tainted=true. The runner's exec layer (S41d) refuses to interpolate
// tainted values into shell strings — they compile to ${SHITHUB_INPUT_*}
// envvar references set safely by the runner. This is the load-bearing
// contract for expression-injection prevention.
package workflow

// Workflow is the parsed top-level document.
type Workflow struct {
	// Name is the optional human-readable name (`name:` at root). When
	// blank, the UI defaults to the workflow file's basename.
	Name string

	// On lists trigger predicates. The trigger pipeline (S41b) matches
	// domain_events against these.
	On TriggerSet

	// Permissions is the workflow-level permissions block; jobs may
	// further narrow but cannot widen.
	Permissions Permissions

	// Env is the workflow-level env map. Values may carry expressions;
	// resolution + taint propagation happen at dispatch time.
	Env map[string]Value

	// Concurrency is the workflow-level concurrency control. Honored
	// from S41g; carried in v1 so the schema doesn't churn.
	Concurrency Concurrency

	// Jobs is keyed by the job's id (`jobs.<key>:`). Order is the YAML
	// document order so re-rendering matches the author's layout.
	Jobs []Job
}

// TriggerSet is the parsed `on:` block. We support: push, pull_request,
// schedule (cron), workflow_dispatch.
type TriggerSet struct {
	Push             *PushTrigger
	PullRequest      *PullRequestTrigger
	Schedule         []ScheduleTrigger
	WorkflowDispatch *WorkflowDispatchTrigger
}

// PushTrigger filters which pushes match. Empty Branches/Tags/Paths
// means "all"; non-empty applies the standard GHA glob semantics.
// Both include and exclude (negative-glob with leading !) are accepted
// and are consulted in declaration order.
type PushTrigger struct {
	Branches []string
	Tags     []string
	Paths    []string
}

// PullRequestTrigger filters which PR events match. Types is the
// list of activity types (opened, synchronize, reopened, …).
type PullRequestTrigger struct {
	Types    []string
	Branches []string
	Paths    []string
}

// ScheduleTrigger declares a single cron entry. Multiple entries are
// allowed; each fires independently.
type ScheduleTrigger struct {
	Cron string
}

// WorkflowDispatchTrigger declares the manual-trigger surface. Inputs
// are typed parameters the dispatcher prompts for.
type WorkflowDispatchTrigger struct {
	Inputs []DispatchInput
}

// DispatchInput is a single typed input for workflow_dispatch. v1
// accepts string, boolean, choice, environment.
type DispatchInput struct {
	Name        string
	Description string
	Type        string // "string" | "boolean" | "choice" | "environment"
	Default     string
	Required    bool
	Options     []string // populated when Type=="choice"
}

// Permissions is the GitHub-Actions-equivalent permissions block.
// Empty (zero) value means "default" which is read for content.
// Specific keys mirror GHA names: contents, pull-requests, issues,
// actions, deployments, packages, statuses, security-events, etc.
//
// A workflow may set Permissions: {} to deny all, or {Mode: "all"} to
// grant all (subject to the actor having repo write).
type Permissions struct {
	Mode string                     // "" | "read-all" | "write-all" | "none"
	Per  map[string]PermissionLevel // keyed by GHA permission name
}

// PermissionLevel mirrors GHA's per-permission grants.
type PermissionLevel string

const (
	PermissionNone  PermissionLevel = "none"
	PermissionRead  PermissionLevel = "read"
	PermissionWrite PermissionLevel = "write"
)

// Concurrency is the workflow-level concurrency control. Group is an
// expression evaluated against the trigger context to produce the
// group key. CancelInProgress=true cancels older runs for the same
// group when a new one is enqueued.
type Concurrency struct {
	Group            Value
	CancelInProgress bool
}

// Job is one entry under `jobs:`.
type Job struct {
	// Key is the YAML map key: `jobs.<key>:`. Identifier-shape, used
	// for `needs:` references and as the URL slug.
	Key string

	// Name is the optional human-readable name (`jobs.<key>.name:`).
	// Falls back to Key when blank.
	Name string

	// RunsOn is the runner-selector string ("ubuntu-latest", "self-hosted",
	// "nix-flake", etc.). Mapped at runner-claim time to the actual
	// container image / engine.
	RunsOn string

	// Needs lists job keys this job depends on. The trigger pipeline
	// resolves these to job IDs at insert time and the runner respects
	// them at dispatch time (S41b/d).
	Needs []string

	// If is the job-level conditional. Evaluated against the trigger
	// context just before dispatch; false → skipped.
	If string

	// TimeoutMinutes bounds total job runtime. Default 360 (6h),
	// matches GHA. Range 1-4320 enforced by the parser.
	TimeoutMinutes int

	// Permissions narrows the workflow-level permissions for this job.
	// Cannot widen.
	Permissions Permissions

	// Environment is the optional GitHub-compatible deployment
	// environment declared by `jobs.<key>.environment`. When set, runner
	// dispatch may layer environment-scoped secrets and later SP23
	// protection rules before execution.
	Environment Environment

	// Env is per-job env overlay. Merged on top of workflow Env.
	Env map[string]Value

	// Steps run serially. Order is YAML document order.
	Steps []Step
}

// Environment is a job's optional deployment environment. GitHub accepts
// either a scalar (`environment: production`) or a mapping with `name` and
// `url`; we keep both raw because expression resolution is runtime-owned.
type Environment struct {
	Name string
	URL  Value
}

// Step is one entry under a job's `steps:`.
type Step struct {
	// ID is the optional `id:` for cross-step references via
	// ${{ steps.<id>.outputs.X }}. Outputs themselves are v2; we carry
	// the id field now so the schema doesn't churn.
	ID string

	// Name is the human-readable label. Falls back to a synthesized
	// "Run <first-line-of-run-command>" when blank.
	Name string

	// If is the step-level conditional. Evaluated mid-job.
	If string

	// Run is the shell command. Empty when this step uses an alias.
	// May contain `${{ … }}` expressions; the parser resolves them
	// into Value{Tainted: …} nodes.
	Run string

	// Uses is the magic-alias slug. Exactly one of Run or Uses is
	// non-empty per the migration's CHECK constraint.
	Uses string

	// With is the input map for `uses:` aliases. Forwarded to the
	// alias-specific runner step (e.g., upload-artifact's `name:`).
	With map[string]Value

	// WorkingDirectory overrides the step's cwd.
	WorkingDirectory string

	// Env is per-step env overlay. Merged on top of job Env.
	Env map[string]Value

	// ContinueOnError lets the job proceed past this step's failure.
	ContinueOnError bool
}

// Value is a parsed value carried in the workflow tree (env entries,
// `with:` inputs, concurrency-group expressions). At parse time we
// only know the raw source string — the taint determination happens
// at expression-evaluation time inside `internal/actions/expr` when
// the runner resolves a reference against the trigger context. The
// runner-side `expr.Value` carries the load-bearing `Tainted bool`.
//
// Pre-L5 this struct also had a `Tainted bool` field plus a `Tainted()`
// constructor — both unused (the parser only ever called `V()`). The
// duplication confused readers because two different `Value` types
// claimed to own the taint contract; the architecture doc has always
// described `expr.Value.Tainted` as load-bearing. Single source of
// truth now: this struct just carries `Raw`; taint lives in
// `expr.Value.Tainted` exclusively.
type Value struct {
	Raw string
}

// V wraps a raw source string into a parser-side Value. The parser
// uses this when it carries a literal or expression body verbatim
// from the YAML — taint resolution is a runtime concern.
func V(raw string) Value { return Value{Raw: raw} }

// Diagnostic is a parser finding. Severity controls whether parsing
// continues; Path is dot-notated for UI display ("jobs.test.steps[2].run").
type Diagnostic struct {
	Path     string
	Message  string
	Severity Severity
}

// Severity classifies a diagnostic.
type Severity int

const (
	// Error stops parsing — the workflow is unusable.
	Error Severity = iota

	// Warning is non-fatal but flagged in the UI. Used for the
	// `${{ github.* }}` deprecation alias.
	Warning
)

// String renders the diagnostic for the admin parse subcommand and
// future UI surfaces.
func (d Diagnostic) String() string {
	prefix := "error"
	if d.Severity == Warning {
		prefix = "warning"
	}
	if d.Path == "" {
		return prefix + ": " + d.Message
	}
	return prefix + " at " + d.Path + ": " + d.Message
}

// MaxWorkflowFileBytes is the parser-side size cap. Files larger than
// this are rejected before YAML decode begins. 64 KB is the GHA limit
// minus a small margin.
const MaxWorkflowFileBytes = 64 * 1024

// MaxYAMLAliases bounds anchor expansions per document — the
// billion-laughs guard. yaml.v3 doesn't expose a direct knob; we
// track aliases at decode time via a custom Unmarshaler in parse.go
// and cap at this value.
const MaxYAMLAliases = 100
