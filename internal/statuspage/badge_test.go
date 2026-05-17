// SPDX-License-Identifier: AGPL-3.0-or-later

package statuspage

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// TestBadgeWellFormedXML asserts every state produces parseable XML +
// the expected color + the expected right-side text. Regressions in
// renderBadge that escape characters wrong (or skip them) would either
// fail xml.Unmarshal or fail the substring assertions.
func TestBadgeWellFormedXML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		state     State
		wantColor string
		wantText  string
	}{
		{"ok", StateOK, colorOK, "ok"},
		{"degraded", StateDegraded, colorDegraded, "degraded"},
		{"down", StateDown, colorDown, "down"},
		{"unknown", StateUnknown, colorUnknown, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svg := BuildBadge(tc.state)
			if !bytes.HasPrefix(svg, []byte(`<?xml`)) {
				t.Fatalf("missing XML declaration: %s", svg)
			}
			body := bytes.TrimPrefix(svg, []byte(`<?xml version="1.0" encoding="UTF-8"?>`))
			var probe struct {
				XMLName xml.Name `xml:"svg"`
			}
			if err := xml.Unmarshal(body, &probe); err != nil {
				t.Fatalf("xml unmarshal: %v\nbody: %s", err, body)
			}
			if probe.XMLName.Local != "svg" {
				t.Fatalf("root element = %q, want svg", probe.XMLName.Local)
			}
			s := string(svg)
			if !strings.Contains(s, tc.wantColor) {
				t.Errorf("missing color %q in svg: %s", tc.wantColor, s)
			}
			if !strings.Contains(s, ">"+tc.wantText+"<") {
				t.Errorf("missing rendered text %q in svg", tc.wantText)
			}
		})
	}
}

// TestPaidBadge asserts the 402-style teaser badge uses the brand
// blue + says "Pro feature". This is the badge Free users see when
// the enforce flag is on; a regression that lights up "ok" green
// would silently leak a misleading status signal.
func TestPaidBadge(t *testing.T) {
	t.Parallel()
	svg := BuildPaidBadge()
	s := string(svg)
	if !strings.Contains(s, colorPaid) {
		t.Errorf("paid badge missing blue color %q", colorPaid)
	}
	if !strings.Contains(s, ">Pro feature<") {
		t.Errorf("paid badge missing 'Pro feature' text")
	}
}

// TestBadgeEscapesLabel is the security regression: a caller-supplied
// label string with `<` must NOT close the SVG element. xmlEscape
// catches it; this test guards future refactors that might use raw
// concatenation.
func TestBadgeEscapesLabel(t *testing.T) {
	t.Parallel()
	svg := BuildBadgeWithLabel(`</text><script>alert(1)</script>`, StateOK)
	s := string(svg)
	if strings.Contains(s, "<script>") {
		t.Fatalf("unescaped <script>: %s", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("expected escaped &lt;script&gt; in svg: %s", s)
	}
}

// TestXMLEscapeFastPath asserts ASCII strings without meta-chars
// take the fast path and are returned unchanged. Catches a
// regression that runs every string through the slow path.
func TestXMLEscapeFastPath(t *testing.T) {
	t.Parallel()
	in := "abc-123_xyz"
	if got := xmlEscape(in); got != in {
		t.Errorf("fast path returned %q, want %q", got, in)
	}
}
