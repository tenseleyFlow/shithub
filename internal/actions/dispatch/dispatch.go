// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dispatch holds the input-validation primitives shared by
// the HTML and REST workflow_dispatch handlers. Both surfaces accept
// a `(workflow_file, ref, inputs)` triple and need to:
//
//   - Validate the workflow_file path is well-formed and inside
//     `.shithub/workflows/`.
//   - Normalize the caller's inputs against the workflow's declared
//     `on.workflow_dispatch.inputs` (booleans become "true"/"false"
//     strings, required inputs are enforced, choice options validated,
//     unknown input names rejected).
//   - Parse `inputs.<name>` form fields when the request is
//     form-encoded.
//
// Keeping these helpers in their own package means the HTML + REST
// handlers can't drift on workflow_dispatch semantics — both feed
// through the same code path.
package dispatch

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

// WorkflowFilesDir is the canonical location of shithub workflow files
// inside a repo. Exported so both surfaces share the literal.
const WorkflowFilesDir = ".shithub/workflows/"

// NormalizeFilePath accepts either a basename ("ci.yml") or a full
// repo-relative path (".shithub/workflows/ci.yml") and returns the
// canonical full path. Returns ErrInvalidWorkflowName if the result
// would escape `.shithub/workflows/` or doesn't end in `.yml`/`.yaml`.
func NormalizeFilePath(file string) (string, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", ErrInvalidWorkflowName
	}
	if !strings.HasPrefix(file, WorkflowFilesDir) {
		file = WorkflowFilesDir + file
	}
	if strings.Contains(file, "..") || !ValidWorkflowName(file) {
		return "", ErrInvalidWorkflowName
	}
	return file, nil
}

// ValidWorkflowName guards against URL parameter shenanigans by
// requiring the resolved file path to look like
// `.shithub/workflows/<basename>.{yml,yaml}` with no path traversal.
func ValidWorkflowName(file string) bool {
	if !strings.HasPrefix(file, WorkflowFilesDir) {
		return false
	}
	rest := strings.TrimPrefix(file, WorkflowFilesDir)
	if rest == "" || strings.ContainsAny(rest, "/\x00") {
		return false
	}
	return strings.HasSuffix(rest, ".yml") || strings.HasSuffix(rest, ".yaml")
}

// ErrInvalidWorkflowName signals a malformed workflow_file path.
// Callers map this to a 400-shaped response.
var ErrInvalidWorkflowName = fmt.Errorf("dispatch: invalid workflow file path")

// NormalizeInputs validates and normalises caller-supplied inputs
// against a workflow's declared `on.workflow_dispatch.inputs`.
//
//   - Unknown input names → error.
//   - Missing required (non-boolean) inputs → error.
//   - Booleans must be "true"/"false"; missing booleans default to
//     "false" (matches GHA semantics).
//   - Choices must match one of the declared options.
//   - Empty optional inputs with a default get the default applied.
//
// The returned map is the value the trigger pipeline records on the
// workflow run; nil is valid (means "no inputs apply").
func NormalizeInputs(raw map[string]string, specs []workflow.DispatchInput) (map[string]string, error) {
	if raw == nil {
		raw = map[string]string{}
	}
	known := make(map[string]workflow.DispatchInput, len(specs))
	for _, spec := range specs {
		known[spec.Name] = spec
	}
	for name := range raw {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("unknown workflow_dispatch input %q", name)
		}
	}

	out := make(map[string]string, len(specs))
	for _, spec := range specs {
		value, provided := raw[spec.Name]
		if !provided || value == "" {
			value = spec.Default
		}
		if spec.Type == "boolean" && !provided && value == "" {
			value = "false"
		}
		if spec.Required && spec.Type != "boolean" && strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("workflow_dispatch input %q is required", spec.Name)
		}
		switch spec.Type {
		case "boolean":
			if value != "true" && value != "false" {
				return nil, fmt.Errorf("workflow_dispatch input %q must be true or false", spec.Name)
			}
		case "choice":
			if value != "" && !choiceAllowed(value, spec.Options) {
				return nil, fmt.Errorf("workflow_dispatch input %q must be one of the declared options", spec.Name)
			}
		}
		if value != "" || spec.Type == "boolean" {
			out[spec.Name] = value
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// InputsFromForm pulls `inputs.<name>` keys out of a form-encoded
// body. The HTML dispatch UI submits inputs this way; the REST
// surface prefers JSON but also accepts this shape so curl ergonomics
// work without a JSON body.
func InputsFromForm(values url.Values) map[string]string {
	inputs := make(map[string]string)
	for key, vals := range values {
		name, ok := strings.CutPrefix(key, "inputs.")
		if !ok || name == "" || len(vals) == 0 {
			continue
		}
		inputs[name] = vals[len(vals)-1]
	}
	if len(inputs) == 0 {
		return nil
	}
	return inputs
}

func choiceAllowed(value string, options []string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
