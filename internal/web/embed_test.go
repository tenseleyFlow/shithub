// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// TestProductionTemplatesParse runs the embedded production templates
// through render.New so any undefined-template ref or unparseable file
// fails CI rather than the binary's first request after deploy.
//
// The check exists in addition to the targeted unit tests in
// internal/web/render/render_test.go: those cover the validator's
// behaviour on synthetic FS trees; this one covers the actual files
// under internal/web/templates/.
func TestProductionTemplatesParse(t *testing.T) {
	t.Parallel()
	_, err := render.New(TemplatesFS(), render.Options{})
	if err != nil {
		t.Fatalf("production templates failed to parse: %v", err)
	}
}
