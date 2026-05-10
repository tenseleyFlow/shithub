// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrTooLarge is returned when the workflow file exceeds
// MaxWorkflowFileBytes. The cap is enforced before YAML decode so a
// malicious file can't blow the parser's memory budget.
var ErrTooLarge = errors.New("workflow file exceeds size limit")

// ErrTooManyAliases is returned when a YAML document expands more
// than MaxYAMLAliases anchor references — the billion-laughs guard.
var ErrTooManyAliases = errors.New("workflow YAML has too many aliases (anchor-bomb guard)")

// Parse decodes a workflow file. It returns the parsed document, the
// list of diagnostics encountered (warnings non-fatal, errors fatal),
// and an error iff the file was unparseable.
//
// The parser is strict: unknown top-level keys, unknown step keys,
// and `uses:` references outside the AllowedUsesAliases set all
// produce diagnostics with Severity=Error and the function returns
// (nil, diagnostics, nil). Callers (S41b trigger pipeline,
// `shithubd admin actions parse` CLI) decide what to do with that.
//
// On a YAML-level error (malformed syntax, anchor bomb, oversized
// file), Parse returns (nil, diagnostics, err).
func Parse(src []byte) (*Workflow, []Diagnostic, error) {
	if len(src) > MaxWorkflowFileBytes {
		return nil, []Diagnostic{{
			Message:  fmt.Sprintf("workflow file is %d bytes; limit is %d", len(src), MaxWorkflowFileBytes),
			Severity: Error,
		}}, ErrTooLarge
	}

	// We decode first to a yaml.Node so we can preserve doc order and
	// catch anchor abuse. Then we hand-walk into the typed Workflow.
	var root yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	if err := dec.Decode(&root); err != nil {
		return nil, []Diagnostic{{
			Message:  "YAML decode: " + err.Error(),
			Severity: Error,
		}}, err
	}
	if root.Kind == 0 {
		return nil, []Diagnostic{{
			Message:  "workflow file is empty",
			Severity: Error,
		}}, errors.New("empty workflow")
	}
	if root.Kind != yaml.DocumentNode {
		return nil, []Diagnostic{{
			Message:  "expected YAML document at root",
			Severity: Error,
		}}, errors.New("non-document root")
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, []Diagnostic{{
			Message:  "workflow must be a YAML mapping at the top level",
			Severity: Error,
		}}, errors.New("non-mapping root")
	}
	if aliases := countAliases(root.Content[0], 0); aliases > MaxYAMLAliases {
		return nil, []Diagnostic{{
			Message:  fmt.Sprintf("workflow has %d alias references; limit is %d", aliases, MaxYAMLAliases),
			Severity: Error,
		}}, ErrTooManyAliases
	}

	w := &Workflow{
		Env:  map[string]Value{},
		Jobs: nil,
	}
	var diags []Diagnostic
	mapping := root.Content[0]

	// Top-level keys are walked deterministically. Unknown keys produce
	// diagnostics so workflow authors catch typos at parse time.
	for i := 0; i < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		v := mapping.Content[i+1]
		switch k.Value {
		case "name":
			if v.Kind != yaml.ScalarNode {
				diags = append(diags, errAt("name", "must be a scalar string"))
				continue
			}
			w.Name = v.Value
		case "on":
			ts, ds := parseOn(v)
			w.On = ts
			diags = append(diags, ds...)
		case "permissions":
			perms, ds := parsePermissions(v, "permissions")
			w.Permissions = perms
			diags = append(diags, ds...)
		case "env":
			env, ds := parseEnv(v, "env")
			w.Env = env
			diags = append(diags, ds...)
		case "concurrency":
			c, ds := parseConcurrency(v, "concurrency")
			w.Concurrency = c
			diags = append(diags, ds...)
		case "jobs":
			jobs, ds := parseJobs(v)
			w.Jobs = jobs
			diags = append(diags, ds...)
		default:
			diags = append(diags, errAt(k.Value, "unknown top-level key (allowed: name, on, permissions, env, concurrency, jobs)"))
		}
	}

	if len(w.Jobs) == 0 && !hasError(diags) {
		diags = append(diags, errAt("jobs", "workflow must declare at least one job"))
	}
	if !triggerSetIsNonEmpty(w.On) && !hasError(diags) {
		diags = append(diags, errAt("on", "workflow must declare at least one trigger"))
	}

	if hasError(diags) {
		return nil, diags, nil
	}
	return w, diags, nil
}

// countAliases walks the YAML node graph and returns the number of
// alias dereferences. Used only as the anchor-bomb guard; we don't
// resolve aliases ourselves (yaml.v3 does that during Decode).
func countAliases(n *yaml.Node, depth int) int {
	if n == nil || depth > 100 {
		return 0
	}
	count := 0
	if n.Kind == yaml.AliasNode {
		count++
	}
	for _, c := range n.Content {
		count += countAliases(c, depth+1)
		if count > MaxYAMLAliases {
			return count
		}
	}
	return count
}

// parseOn handles the `on:` block in its three documented shapes:
//   - shorthand string: `on: push`
//   - shorthand list: `on: [push, pull_request]`
//   - mapping: `on: { push: { branches: [main] }, schedule: [...] }`
func parseOn(n *yaml.Node) (TriggerSet, []Diagnostic) {
	var ts TriggerSet
	var diags []Diagnostic
	switch n.Kind {
	case yaml.ScalarNode:
		applyEventName(&ts, n.Value, &diags, "on")
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				diags = append(diags, errAt("on", "list items must be event names"))
				continue
			}
			applyEventName(&ts, item.Value, &diags, "on")
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			switch k.Value {
			case "push":
				ts.Push = parsePushTrigger(v, &diags)
			case "pull_request":
				ts.PullRequest = parsePullRequestTrigger(v, &diags)
			case "schedule":
				ts.Schedule = parseScheduleTriggers(v, &diags)
			case "workflow_dispatch":
				ts.WorkflowDispatch = parseDispatchTrigger(v, &diags)
			default:
				diags = append(diags, errAt("on."+k.Value, "unknown event type (allowed: push, pull_request, schedule, workflow_dispatch)"))
			}
		}
	default:
		diags = append(diags, errAt("on", "must be a string, sequence, or mapping"))
	}
	return ts, diags
}

func applyEventName(ts *TriggerSet, name string, diags *[]Diagnostic, path string) {
	switch name {
	case "push":
		if ts.Push == nil {
			ts.Push = &PushTrigger{}
		}
	case "pull_request":
		if ts.PullRequest == nil {
			ts.PullRequest = &PullRequestTrigger{}
		}
	case "workflow_dispatch":
		if ts.WorkflowDispatch == nil {
			ts.WorkflowDispatch = &WorkflowDispatchTrigger{}
		}
	default:
		*diags = append(*diags, errAt(path, "unknown event "+strconv.Quote(name)+" (allowed: push, pull_request, workflow_dispatch — schedule requires the mapping form)"))
	}
}

func parsePushTrigger(n *yaml.Node, diags *[]Diagnostic) *PushTrigger {
	pt := &PushTrigger{}
	if n.Kind == yaml.ScalarNode && n.Value == "" {
		return pt
	}
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt("on.push", "must be a mapping"))
		return pt
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		switch k.Value {
		case "branches":
			pt.Branches = scalarList(v, "on.push.branches", diags)
		case "tags":
			pt.Tags = scalarList(v, "on.push.tags", diags)
		case "paths":
			pt.Paths = scalarList(v, "on.push.paths", diags)
		default:
			*diags = append(*diags, errAt("on.push."+k.Value, "unknown push filter (allowed: branches, tags, paths)"))
		}
	}
	return pt
}

func parsePullRequestTrigger(n *yaml.Node, diags *[]Diagnostic) *PullRequestTrigger {
	prt := &PullRequestTrigger{}
	if n.Kind == yaml.ScalarNode && n.Value == "" {
		return prt
	}
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt("on.pull_request", "must be a mapping"))
		return prt
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		switch k.Value {
		case "types":
			prt.Types = scalarList(v, "on.pull_request.types", diags)
		case "branches":
			prt.Branches = scalarList(v, "on.pull_request.branches", diags)
		case "paths":
			prt.Paths = scalarList(v, "on.pull_request.paths", diags)
		default:
			*diags = append(*diags, errAt("on.pull_request."+k.Value, "unknown filter (allowed: types, branches, paths)"))
		}
	}
	return prt
}

func parseScheduleTriggers(n *yaml.Node, diags *[]Diagnostic) []ScheduleTrigger {
	if n.Kind != yaml.SequenceNode {
		*diags = append(*diags, errAt("on.schedule", "must be a sequence of cron entries"))
		return nil
	}
	out := make([]ScheduleTrigger, 0, len(n.Content))
	for i, entry := range n.Content {
		if entry.Kind != yaml.MappingNode {
			*diags = append(*diags, errAt(fmt.Sprintf("on.schedule[%d]", i), "must be a mapping with a `cron:` key"))
			continue
		}
		var s ScheduleTrigger
		for j := 0; j < len(entry.Content); j += 2 {
			k := entry.Content[j]
			v := entry.Content[j+1]
			switch k.Value {
			case "cron":
				if v.Kind != yaml.ScalarNode {
					*diags = append(*diags, errAt(fmt.Sprintf("on.schedule[%d].cron", i), "must be a scalar cron expression"))
					continue
				}
				s.Cron = v.Value
			default:
				*diags = append(*diags, errAt(fmt.Sprintf("on.schedule[%d].%s", i, k.Value), "unknown schedule key (allowed: cron)"))
			}
		}
		if s.Cron == "" {
			*diags = append(*diags, errAt(fmt.Sprintf("on.schedule[%d]", i), "missing required cron expression"))
			continue
		}
		out = append(out, s)
	}
	return out
}

func parseDispatchTrigger(n *yaml.Node, diags *[]Diagnostic) *WorkflowDispatchTrigger {
	wdt := &WorkflowDispatchTrigger{}
	if n.Kind == yaml.ScalarNode && n.Value == "" {
		return wdt
	}
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt("on.workflow_dispatch", "must be a mapping"))
		return wdt
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		switch k.Value {
		case "inputs":
			wdt.Inputs = parseDispatchInputs(v, diags)
		default:
			*diags = append(*diags, errAt("on.workflow_dispatch."+k.Value, "unknown dispatch key (allowed: inputs)"))
		}
	}
	return wdt
}

func parseDispatchInputs(n *yaml.Node, diags *[]Diagnostic) []DispatchInput {
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt("on.workflow_dispatch.inputs", "must be a mapping of input-name → spec"))
		return nil
	}
	out := make([]DispatchInput, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		nameNode := n.Content[i]
		specNode := n.Content[i+1]
		input := DispatchInput{Name: nameNode.Value}
		if specNode.Kind != yaml.MappingNode {
			*diags = append(*diags, errAt("on.workflow_dispatch.inputs."+nameNode.Value, "must be a mapping"))
			continue
		}
		for j := 0; j < len(specNode.Content); j += 2 {
			k := specNode.Content[j]
			v := specNode.Content[j+1]
			switch k.Value {
			case "description":
				input.Description = v.Value
			case "type":
				input.Type = v.Value
			case "default":
				input.Default = v.Value
			case "required":
				input.Required = parseBool(v, "on.workflow_dispatch.inputs."+nameNode.Value+".required", diags)
			case "options":
				input.Options = scalarList(v, "on.workflow_dispatch.inputs."+nameNode.Value+".options", diags)
			default:
				*diags = append(*diags, errAt("on.workflow_dispatch.inputs."+nameNode.Value+"."+k.Value, "unknown input key"))
			}
		}
		if input.Type == "" {
			input.Type = "string"
		}
		out = append(out, input)
	}
	return out
}

func parsePermissions(n *yaml.Node, path string) (Permissions, []Diagnostic) {
	var diags []Diagnostic
	p := Permissions{Per: map[string]PermissionLevel{}}
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Value {
		case "read-all", "write-all", "none":
			p.Mode = n.Value
		default:
			diags = append(diags, errAt(path, "unknown shorthand (allowed: read-all, write-all, none)"))
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			lvl := PermissionLevel(v.Value)
			if lvl != PermissionNone && lvl != PermissionRead && lvl != PermissionWrite {
				diags = append(diags, errAt(path+"."+k.Value, "permission level must be none, read, or write"))
				continue
			}
			p.Per[k.Value] = lvl
		}
	default:
		diags = append(diags, errAt(path, "must be a shorthand string or a mapping"))
	}
	return p, diags
}

func parseEnv(n *yaml.Node, path string) (map[string]Value, []Diagnostic) {
	var diags []Diagnostic
	if n.Kind != yaml.MappingNode {
		diags = append(diags, errAt(path, "must be a mapping"))
		return nil, diags
	}
	out := map[string]Value{}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			diags = append(diags, errAt(path+"."+k.Value, "env values must be scalars"))
			continue
		}
		// We tag env values literal-trusted here. The expression
		// evaluator (S41a expr/eval.go) walks the Raw at dispatch
		// time and propagates taint when the value contains
		// `${{ shithub.event.X }}` references.
		out[k.Value] = V(v.Value)
	}
	return out, diags
}

func parseConcurrency(n *yaml.Node, path string) (Concurrency, []Diagnostic) {
	var diags []Diagnostic
	c := Concurrency{}
	switch n.Kind {
	case yaml.ScalarNode:
		c.Group = V(n.Value)
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			switch k.Value {
			case "group":
				c.Group = V(v.Value)
			case "cancel-in-progress":
				c.CancelInProgress = parseBool(v, path+".cancel-in-progress", &diags)
			default:
				diags = append(diags, errAt(path+"."+k.Value, "unknown concurrency key (allowed: group, cancel-in-progress)"))
			}
		}
	default:
		diags = append(diags, errAt(path, "must be a string or mapping"))
	}
	return c, diags
}

func parseJobs(n *yaml.Node) ([]Job, []Diagnostic) {
	var diags []Diagnostic
	if n.Kind != yaml.MappingNode {
		diags = append(diags, errAt("jobs", "must be a mapping of job-key → job-spec"))
		return nil, diags
	}
	jobs := make([]Job, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		j, ds := parseJob(k.Value, v)
		diags = append(diags, ds...)
		jobs = append(jobs, j)
	}
	return jobs, diags
}

func parseJob(key string, n *yaml.Node) (Job, []Diagnostic) {
	var diags []Diagnostic
	j := Job{Key: key, TimeoutMinutes: 360}
	if n.Kind != yaml.MappingNode {
		diags = append(diags, errAt("jobs."+key, "job spec must be a mapping"))
		return j, diags
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		path := "jobs." + key + "." + k.Value
		switch k.Value {
		case "name":
			j.Name = v.Value
		case "runs-on":
			j.RunsOn = v.Value
		case "needs":
			if v.Kind == yaml.ScalarNode {
				j.Needs = []string{v.Value}
			} else {
				j.Needs = scalarList(v, path, &diags)
			}
		case "if":
			j.If = v.Value
		case "timeout-minutes":
			n, err := strconv.Atoi(v.Value)
			if err != nil || n < 1 || n > 4320 {
				diags = append(diags, errAt(path, "timeout-minutes must be an integer 1-4320"))
				continue
			}
			j.TimeoutMinutes = n
		case "permissions":
			p, ds := parsePermissions(v, path)
			j.Permissions = p
			diags = append(diags, ds...)
		case "env":
			env, ds := parseEnv(v, path)
			j.Env = env
			diags = append(diags, ds...)
		case "steps":
			steps, ds := parseSteps(v, "jobs."+key)
			j.Steps = steps
			diags = append(diags, ds...)
		default:
			diags = append(diags, errAt(path, "unknown job key (allowed: name, runs-on, needs, if, timeout-minutes, permissions, env, steps)"))
		}
	}
	if j.RunsOn == "" {
		diags = append(diags, errAt("jobs."+key, "job missing required `runs-on:`"))
	}
	if len(j.Steps) == 0 {
		diags = append(diags, errAt("jobs."+key, "job has no steps"))
	}
	return j, diags
}

func parseSteps(n *yaml.Node, jobPath string) ([]Step, []Diagnostic) {
	var diags []Diagnostic
	if n.Kind != yaml.SequenceNode {
		diags = append(diags, errAt(jobPath+".steps", "must be a sequence"))
		return nil, diags
	}
	steps := make([]Step, 0, len(n.Content))
	for idx, item := range n.Content {
		s, ds := parseStep(idx, item, jobPath)
		diags = append(diags, ds...)
		steps = append(steps, s)
	}
	return steps, diags
}

func parseStep(idx int, n *yaml.Node, jobPath string) (Step, []Diagnostic) {
	var diags []Diagnostic
	s := Step{}
	stepPath := fmt.Sprintf("%s.steps[%d]", jobPath, idx)
	if n.Kind != yaml.MappingNode {
		diags = append(diags, errAt(stepPath, "step must be a mapping"))
		return s, diags
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		path := stepPath + "." + k.Value
		switch k.Value {
		case "id":
			s.ID = v.Value
		case "name":
			s.Name = v.Value
		case "if":
			s.If = v.Value
		case "run":
			s.Run = v.Value
		case "uses":
			s.Uses = v.Value
		case "with":
			env, ds := parseEnv(v, path)
			s.With = env
			diags = append(diags, ds...)
		case "working-directory":
			s.WorkingDirectory = v.Value
		case "env":
			env, ds := parseEnv(v, path)
			s.Env = env
			diags = append(diags, ds...)
		case "continue-on-error":
			s.ContinueOnError = parseBool(v, path, &diags)
		default:
			diags = append(diags, errAt(path, "unknown step key (allowed: id, name, if, run, uses, with, working-directory, env, continue-on-error)"))
		}
	}
	if s.Run == "" && s.Uses == "" {
		diags = append(diags, errAt(stepPath, "step must have either `run:` or `uses:`"))
	}
	if s.Run != "" && s.Uses != "" {
		diags = append(diags, errAt(stepPath, "step cannot have both `run:` and `uses:`"))
	}
	if s.Uses != "" && !IsAllowedUses(s.Uses) {
		diags = append(diags, errAt(stepPath+".uses",
			"unsupported `uses:` reference; v1 supports only "+
				"actions/checkout@v4, shithub/upload-artifact@v1, shithub/download-artifact@v1"))
	}
	return s, diags
}

// scalarList parses either a single scalar or a sequence of scalars
// into a []string. Used for branches/tags/paths/types-style lists.
func scalarList(n *yaml.Node, path string, diags *[]Diagnostic) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				*diags = append(*diags, errAt(path, "list items must be scalars"))
				continue
			}
			out = append(out, item.Value)
		}
		return out
	default:
		*diags = append(*diags, errAt(path, "must be a string or sequence of strings"))
		return nil
	}
}

func errAt(path, msg string) Diagnostic {
	return Diagnostic{Path: path, Message: msg, Severity: Error}
}

// parseBool resolves a YAML scalar to a bool. Pre-L1 the parser used
// `n.Value == "true"` which silently mishandled `True`/`TRUE` (both
// valid YAML 1.2 booleans) by falling to false. strconv.ParseBool
// accepts the canonical bool literals — true, True, TRUE, false,
// False, FALSE — plus 1/0 and t/f.
//
// On a non-bool value we surface a parse-time diagnostic and return
// false. Authors get a precise error instead of a silently wrong
// flag setting (which used to manifest as `cancel-in-progress: True`
// quietly evaluating false and breaking the concurrency-cancel
// behavior).
func parseBool(n *yaml.Node, path string, diags *[]Diagnostic) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		*diags = append(*diags, errAt(path, "must be a scalar boolean"))
		return false
	}
	b, err := strconv.ParseBool(n.Value)
	if err != nil {
		*diags = append(*diags, errAt(path, "boolean value must be true or false (got "+strconv.Quote(n.Value)+")"))
		return false
	}
	return b
}

// triggerSetIsNonEmpty reports whether at least one trigger is declared.
// TriggerSet contains slices, so it isn't comparable; this helper avoids
// per-call boilerplate at the parse-validate site.
func triggerSetIsNonEmpty(ts TriggerSet) bool {
	return ts.Push != nil || ts.PullRequest != nil ||
		len(ts.Schedule) > 0 || ts.WorkflowDispatch != nil
}

func hasError(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == Error {
			return true
		}
	}
	return false
}
