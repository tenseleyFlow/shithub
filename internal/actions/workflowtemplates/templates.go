// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workflowtemplates owns the curated workflow starter catalogue used
// by the Actions "New workflow" UI. The list is intentionally small: every
// supported template must parse under shithub's v1 workflow dialect.
package workflowtemplates

// Template is one card in the workflow authoring picker.
type Template struct {
	Key         string
	Name        string
	Description string
	Filename    string
	Body        string
	Unsupported bool
	Reason      string
}

var supportedTemplates = []Template{
	{
		Key:         "smoke",
		Name:        "Minimal shell smoke",
		Description: "Run a single shell step on push or manual dispatch.",
		Filename:    "smoke.yml",
		Body: `name: Smoke

on:
  push:
  workflow_dispatch:

jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - name: Smoke
        run: printf 'shithub actions smoke passed\n'
`,
	},
	{
		Key:         "checkout-test",
		Name:        "Checkout plus test",
		Description: "Check out the repository, then run a lightweight file test.",
		Filename:    "checkout.yml",
		Body: `name: Checkout test

on:
  push:
  workflow_dispatch:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Verify checkout
        run: test -f README.md || test -f readme.md || pwd
`,
	},
	{
		Key:         "scheduled-smoke",
		Name:        "Scheduled smoke",
		Description: "Run a smoke command on a cron schedule or manual dispatch.",
		Filename:    "scheduled-smoke.yml",
		Body: `name: Scheduled smoke

on:
  schedule:
    - cron: "0 6 * * *"
  workflow_dispatch:

jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - name: Scheduled smoke
        run: printf 'scheduled shithub smoke passed\n'
`,
	},
	{
		Key:         "manual-dispatch",
		Name:        "Manual dispatch",
		Description: "Create a workflow that only runs when started from the Actions tab.",
		Filename:    "manual.yml",
		Body: `name: Manual smoke

on:
  workflow_dispatch:
    inputs:
      message:
        description: Message to display in the run log
        required: false
        type: string
        default: hello from shithub actions

jobs:
  manual:
    runs-on: ubuntu-latest
    steps:
      - name: Manual smoke
        run: printf 'manual shithub workflow started\n'
`,
	},
}

var unsupportedTemplates = []Template{
	{
		Key:         "go-ci",
		Name:        "Go CI",
		Description: "GitHub's common Go template depends on setup-go and hosted tool cache behavior.",
		Unsupported: true,
		Reason:      "Use a runner image with Go installed and plain run steps until first-party setup/cache support lands.",
	},
	{
		Key:         "node-ci",
		Name:        "Node.js CI",
		Description: "The GitHub template expects setup-node, npm cache restore, and hosted image defaults.",
		Unsupported: true,
		Reason:      "Use explicit run steps on a runner image that already provides Node.js for now.",
	},
	{
		Key:         "matrix-build",
		Name:        "Matrix build",
		Description: "Matrix expansion is not part of the v1 shithub workflow dialect.",
		Unsupported: true,
		Reason:      "Spell out separate jobs explicitly until matrix support is implemented.",
	},
	{
		Key:         "docker-services",
		Name:        "Docker services",
		Description: "Service containers and arbitrary Docker actions require additional sandbox policy.",
		Unsupported: true,
		Reason:      "Keep services outside the workflow or run them inside the operator-controlled runner image.",
	},
}

// Supported returns the runnable starter templates in display order.
func Supported() []Template {
	return cloneTemplates(supportedTemplates)
}

// Unsupported returns honest placeholder cards for common GitHub templates
// that shithub intentionally does not offer as runnable v1 workflows.
func Unsupported() []Template {
	return cloneTemplates(unsupportedTemplates)
}

// Find returns a supported runnable template by catalogue key.
func Find(key string) (Template, bool) {
	for _, tmpl := range supportedTemplates {
		if tmpl.Key == key {
			return tmpl, true
		}
	}
	return Template{}, false
}

func cloneTemplates(in []Template) []Template {
	out := make([]Template, len(in))
	copy(out, in)
	return out
}
