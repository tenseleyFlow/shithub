// SPDX-License-Identifier: AGPL-3.0-or-later

package dispatch_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

func TestNormalizeFilePath_AcceptsBasenameAndFullPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ci.yml", ".shithub/workflows/ci.yml"},
		{".shithub/workflows/ci.yml", ".shithub/workflows/ci.yml"},
		{"  ci.yaml  ", ".shithub/workflows/ci.yaml"},
	}
	for _, tc := range cases {
		got, err := dispatch.NormalizeFilePath(tc.in)
		if err != nil {
			t.Errorf("NormalizeFilePath(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeFilePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeFilePath_RejectsBadInputs(t *testing.T) {
	bad := []string{
		"",
		"../passwd",
		".shithub/workflows/../escape.yml",
		".shithub/workflows/sub/dir.yml",
		".shithub/workflows/ci.txt",
		"foo.txt",
	}
	for _, in := range bad {
		if _, err := dispatch.NormalizeFilePath(in); !errors.Is(err, dispatch.ErrInvalidWorkflowName) {
			t.Errorf("NormalizeFilePath(%q): got err %v, want ErrInvalidWorkflowName", in, err)
		}
	}
}

func TestNormalizeInputs_HappyPath(t *testing.T) {
	specs := []workflow.DispatchInput{
		{Name: "env", Type: "choice", Options: []string{"qa", "prod"}, Default: "qa"},
		{Name: "debug", Type: "boolean"},
		{Name: "ref", Type: "string", Required: true},
	}
	out, err := dispatch.NormalizeInputs(map[string]string{"env": "prod", "ref": "trunk"}, specs)
	if err != nil {
		t.Fatalf("NormalizeInputs: %v", err)
	}
	if out["env"] != "prod" {
		t.Errorf("env: got %q", out["env"])
	}
	// Missing boolean → "false" default.
	if out["debug"] != "false" {
		t.Errorf("debug default: got %q", out["debug"])
	}
	if out["ref"] != "trunk" {
		t.Errorf("ref: got %q", out["ref"])
	}
}

func TestNormalizeInputs_RejectsUnknown(t *testing.T) {
	specs := []workflow.DispatchInput{{Name: "env", Type: "string"}}
	if _, err := dispatch.NormalizeInputs(map[string]string{"bogus": "x"}, specs); err == nil {
		t.Fatal("unknown input accepted")
	}
}

func TestNormalizeInputs_RejectsInvalidChoice(t *testing.T) {
	specs := []workflow.DispatchInput{
		{Name: "env", Type: "choice", Options: []string{"qa", "prod"}},
	}
	if _, err := dispatch.NormalizeInputs(map[string]string{"env": "stage"}, specs); err == nil {
		t.Fatal("invalid choice accepted")
	}
}

func TestNormalizeInputs_RejectsBadBoolean(t *testing.T) {
	specs := []workflow.DispatchInput{{Name: "debug", Type: "boolean"}}
	if _, err := dispatch.NormalizeInputs(map[string]string{"debug": "yes"}, specs); err == nil {
		t.Fatal("non-bool accepted")
	}
}

func TestNormalizeInputs_RequiredEnforced(t *testing.T) {
	specs := []workflow.DispatchInput{{Name: "ref", Type: "string", Required: true}}
	if _, err := dispatch.NormalizeInputs(nil, specs); err == nil {
		t.Fatal("missing required input accepted")
	}
}

func TestNormalizeInputs_AppliesDefault(t *testing.T) {
	specs := []workflow.DispatchInput{{Name: "env", Type: "string", Default: "qa"}}
	out, err := dispatch.NormalizeInputs(nil, specs)
	if err != nil {
		t.Fatalf("NormalizeInputs: %v", err)
	}
	if out["env"] != "qa" {
		t.Errorf("default not applied: %+v", out)
	}
}

func TestInputsFromForm_ParsesInputsPrefix(t *testing.T) {
	v := url.Values{}
	v.Set("ref", "trunk")
	v.Set("inputs.env", "prod")
	v.Set("inputs.debug", "true")
	v.Set("inputs.", "ignored") // empty name → drop
	got := dispatch.InputsFromForm(v)
	if got["env"] != "prod" || got["debug"] != "true" {
		t.Errorf("InputsFromForm: %+v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty input name leaked: %+v", got)
	}
}

func TestInputsFromForm_NoInputsReturnsNil(t *testing.T) {
	v := url.Values{"ref": {"trunk"}}
	if got := dispatch.InputsFromForm(v); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestValidWorkflowName(t *testing.T) {
	good := []string{
		".shithub/workflows/ci.yml",
		".shithub/workflows/release.yaml",
	}
	bad := []string{
		"",
		".shithub/workflows/",
		".shithub/workflows/ci.txt",
		".shithub/workflows/sub/path.yml",
		"foo.yml",
		".shithub/workflows/ci.yml\x00",
	}
	for _, in := range good {
		if !dispatch.ValidWorkflowName(in) {
			t.Errorf("good input rejected: %q", in)
		}
	}
	for _, in := range bad {
		if dispatch.ValidWorkflowName(in) {
			t.Errorf("bad input accepted: %q", in)
		}
	}
}
