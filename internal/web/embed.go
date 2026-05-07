// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"embed"
	"io/fs"
)

//go:embed all:templates
var templatesFS embed.FS

//go:embed all:static
var staticFS embed.FS

// TemplatesFS returns the read-only filesystem rooted at the templates dir.
func TemplatesFS() fs.FS {
	sub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		// embed.FS layout is verified at compile time; this can't fail at
		// runtime once the build succeeds.
		panic(err)
	}
	return sub
}

// StaticFS returns the read-only filesystem rooted at the static dir.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// LogoSVG returns the canonical mascot SVG bytes (full mark, with tentacles
// and pitchfork).
func LogoSVG() ([]byte, error) {
	return staticFS.ReadFile("static/logo/shithub.svg")
}
