// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"strings"
	"testing"
)

func TestOrgActivitySparklineSVG_RendersPolylineGraph(t *testing.T) {
	t.Parallel()
	svg := string(orgActivitySparklineSVG([]int{0, 1, 0, 3}))

	for _, want := range []string{
		`<svg class="shithub-org-repo-spark"`,
		`class="shithub-org-repo-spark-base"`,
		`<polyline class="shithub-org-repo-spark-line"`,
		`points="`,
	} {
		if !strings.Contains(svg, want) {
			t.Fatalf("missing %q in %s", want, svg)
		}
	}
	if strings.Contains(svg, "linear-gradient") {
		t.Fatalf("sparkline should render as SVG geometry, got %s", svg)
	}
}
