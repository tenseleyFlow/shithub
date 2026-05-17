// SPDX-License-Identifier: AGPL-3.0-or-later

package repo_test

// PRO-EXT01-04c: the issue/PR view templates render
// `{{ template "pro-badge" (index $.ProUsernames .Name) }}` next to every
// @author surface. This test pins that idiom: the badge HTML appears
// only when the username is in the map, never for Free authors or
// unknown handles. A regression that flips the conditional would
// silently render the Pro pill for everyone (or no one).

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web"
)

func TestProBadgeMacro_RendersOnlyForKnownProUsernames(t *testing.T) {
	t.Parallel()

	// Load the real pro-badge component shipped to production. If its
	// markup changes (e.g. class rename) and this test isn't updated,
	// every `{{ template "pro-badge" }}` consumer is silently broken.
	badge, err := web.TemplatesFS().Open("_pro_badge.html")
	if err != nil {
		t.Fatalf("open pro_badge: %v", err)
	}
	defer func() { _ = badge.Close() }()
	var badgeBuf bytes.Buffer
	if _, err := badgeBuf.ReadFrom(badge); err != nil {
		t.Fatalf("read pro_badge: %v", err)
	}

	const harness = `{{ range .Authors }}<u>{{ . }}</u>{{ template "pro-badge" (index $.ProUsernames .) }} {{ end }}`
	tmpl, err := template.New("harness").Parse(badgeBuf.String() + harness)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data := map[string]any{
		"Authors": []string{"prouser", "freeuser", "anon"},
		"ProUsernames": map[string]bool{
			"prouser": true,
		},
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	s := out.String()

	const proBadgeMarker = `<span class="shithub-pill shithub-pill-pro"`
	if !strings.Contains(s, `<u>prouser</u>`+proBadgeMarker) {
		t.Errorf("Pro user missing badge: %s", s)
	}
	if strings.Contains(s, `<u>freeuser</u>`+proBadgeMarker) {
		t.Errorf("Free user wrongly got Pro badge: %s", s)
	}
	if strings.Contains(s, `<u>anon</u>`+proBadgeMarker) {
		t.Errorf("Unknown user wrongly got Pro badge: %s", s)
	}
}
