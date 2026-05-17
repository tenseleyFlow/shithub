// SPDX-License-Identifier: AGPL-3.0-or-later

package workflowtemplates_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/actions/workflowtemplates"
)

func TestSupportedTemplatesParse(t *testing.T) {
	t.Parallel()
	for _, tmpl := range workflowtemplates.Supported() {
		t.Run(tmpl.Key, func(t *testing.T) {
			t.Parallel()
			if tmpl.Filename == "" {
				t.Fatal("Filename is empty")
			}
			if tmpl.Body == "" {
				t.Fatal("Body is empty")
			}
			parsed, diags, err := workflow.Parse([]byte(tmpl.Body))
			if err != nil {
				t.Fatalf("Parse returned error: %v diagnostics=%v", err, diags)
			}
			if parsed == nil {
				t.Fatalf("Parse returned nil workflow diagnostics=%v", diags)
			}
			for _, diag := range diags {
				if diag.Severity == workflow.Error {
					t.Fatalf("template has parser error: %v", diag)
				}
			}
		})
	}
}

func TestFindOnlyReturnsSupportedTemplates(t *testing.T) {
	t.Parallel()
	if _, ok := workflowtemplates.Find("smoke"); !ok {
		t.Fatal("Find(smoke) returned ok=false")
	}
	for _, tmpl := range workflowtemplates.Unsupported() {
		if _, ok := workflowtemplates.Find(tmpl.Key); ok {
			t.Fatalf("Find(%q) returned unsupported template", tmpl.Key)
		}
	}
	if _, ok := workflowtemplates.Find("../smoke"); ok {
		t.Fatal("Find accepted path-like key")
	}
}
