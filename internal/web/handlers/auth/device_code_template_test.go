// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// Pinning tests for the device-code consent page. The entry-form UX
// (uppercase auto-correct, paste-aware single-input with dash
// auto-insertion) is the user-facing surface area that defends
// against the device-flow phishing-redirect vector — a future
// template edit must not silently regress it.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func newDeviceCodeRenderer(t *testing.T) *render.Renderer {
	t.Helper()
	rr, err := render.New(web.TemplatesFS(), render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return rr
}

func renderDeviceCode(t *testing.T, rr *render.Renderer, data map[string]any) string {
	t.Helper()
	if _, ok := data["Title"]; !ok {
		data["Title"] = "Authorize device"
	}
	var buf bytes.Buffer
	if err := rr.Render(&buf, "auth/device_code", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestDeviceCodeEntryForm_StylingAndPolish(t *testing.T) {
	t.Parallel()
	rr := newDeviceCodeRenderer(t)
	body := renderDeviceCode(t, rr, map[string]any{})

	// The polish-test asserts on attributes that are load-bearing for
	// the GitHub-style UX nicety: auto-uppercase, paste-friendly,
	// pattern-validated, capped length, monospace via class.
	wants := []string{
		`name="user_code"`,
		`id="shithub-device-code-input"`,
		`class="shithub-device-code-input"`,
		`autocapitalize="characters"`,
		`spellcheck="false"`,
		`maxlength="9"`,
		`pattern="[A-HJ-NP-Za-hj-np-z2-9]{4}-?[A-HJ-NP-Za-hj-np-z2-9]{4}"`,
		`placeholder="XXXX-XXXX"`,
		`autocomplete="off"`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("entry form missing attribute %q", w)
		}
	}

	// Inline JS that normalises the input (uppercase + alphabet
	// filter + dash auto-insert) lives in the same template; assert
	// the load-bearing fragments so a re-format that breaks the
	// regex or the listener wiring fails the test.
	for _, frag := range []string{
		`toUpperCase()`,
		`A-HJ-NP-Z2-9`,
		`addEventListener("input"`,
		`addEventListener("paste"`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("entry form polish script missing %q", frag)
		}
	}

	// Sanity: the screen-reader label is hidden, not removed. The
	// shithub-sr-only class is the project's visually-hidden helper.
	if !strings.Contains(body, `class="shithub-sr-only"`) {
		t.Error("entry form should label the input with a sr-only span")
	}
}

func TestDeviceCodeEntryForm_PreservesPriorUserCode(t *testing.T) {
	t.Parallel()
	rr := newDeviceCodeRenderer(t)
	// When the form rejected a user_code (server returned an error),
	// the input must reprint the prior value so the user can correct
	// it rather than retype from scratch.
	body := renderDeviceCode(t, rr, map[string]any{
		"UserCode": "ABCD-EFGH",
		"Error":    "We don't recognise that code.",
	})
	if !strings.Contains(body, `value="ABCD-EFGH"`) {
		t.Errorf("entry form should preserve prior user_code; body=%q",
			body[:min(len(body), 500)])
	}
	if !strings.Contains(body, "We don&#39;t recognise that code.") &&
		!strings.Contains(body, "We don't recognise that code.") {
		t.Errorf("entry form should render the error flash")
	}
}

func TestDeviceCodeApprovalForm_ShowsUserCodeForVerification(t *testing.T) {
	t.Parallel()
	rr := newDeviceCodeRenderer(t)
	body := renderDeviceCode(t, rr, map[string]any{
		"Approval":  true,
		"ClientID":  "shithub-cli",
		"Scopes":    []string{"user:read", "repo:read"},
		"UserCode":  "ABCD-EFGH",
		"CSRFToken": "test-csrf",
	})

	// The approval page MUST show the user_code prominently so the
	// user can verify it matches the code their CLI displayed.
	// This is the second line of defence against device-flow
	// phishing — even if a user clicks an attacker's pre-filled URL,
	// this line lets them spot a mismatch before approving.
	if !strings.Contains(body, "ABCD-EFGH") {
		t.Error("approval page must show the user_code for CLI-match verification")
	}
	for _, want := range []string{
		`value="approve"`, `value="deny"`, `name="csrf_token"`,
		"shithub-cli",
		"user:read", "repo:read",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approval page missing %q", want)
		}
	}
}
