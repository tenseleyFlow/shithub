// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import "html/template"

// trustedSVG converts a compile-time-constant SVG body into template.HTML.
// Concentrating the conversion in one helper means we have a single point
// where a code reviewer (and the linter) need to verify the input is
// genuinely server-controlled.
//
//nolint:gosec // G203: every caller passes a string literal, never user input.
func trustedSVG(body string) template.HTML {
	return template.HTML(body)
}

// BuiltinOcticons returns a tiny starter set of octicons. S02 ships only
// what the chrome needs (mark-github, three-bars, sun, moon, alert). The
// full Primer catalog is several hundred icons; we vendor on demand rather
// than bloating the binary.
func BuiltinOcticons() OcticonResolver {
	const cls = `width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"`
	icons := map[string]template.HTML{
		"shithub": trustedSVG(`<svg xmlns="http://www.w3.org/2000/svg" ` + cls +
			`><path d="M8 1l1.7 4.4L14 6l-3.5 2.6L11.7 13 8 10.4 4.3 13l1.2-4.4L2 6l4.3-.6L8 1z"/></svg>`),
		"three-bars": trustedSVG(`<svg xmlns="http://www.w3.org/2000/svg" ` + cls +
			`><path d="M1 2.75A.75.75 0 0 1 1.75 2h12.5a.75.75 0 1 1 0 1.5H1.75A.75.75 0 0 1 1 2.75zm0 5A.75.75 0 0 1 1.75 7h12.5a.75.75 0 1 1 0 1.5H1.75A.75.75 0 0 1 1 7.75zm0 5a.75.75 0 0 1 .75-.75h12.5a.75.75 0 1 1 0 1.5H1.75A.75.75 0 0 1 1 12.75z"/></svg>`),
		"sun": trustedSVG(`<svg xmlns="http://www.w3.org/2000/svg" ` + cls +
			`><path d="M8 12a4 4 0 1 1 0-8 4 4 0 0 1 0 8zM8 0a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0V.75A.75.75 0 0 1 8 0zm0 13a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 13zm8-5a.75.75 0 0 1-.75.75h-1.5a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 16 8zM3 8a.75.75 0 0 1-.75.75H.75a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 3 8zm10.657-5.657a.75.75 0 0 1 0 1.06l-1.061 1.062a.75.75 0 1 1-1.06-1.06l1.06-1.062a.75.75 0 0 1 1.061 0zm-9.193 9.193a.75.75 0 0 1 0 1.06l-1.06 1.062a.75.75 0 1 1-1.062-1.061l1.061-1.06a.75.75 0 0 1 1.061-.001zm9.193 1.06a.75.75 0 0 1-1.06 0l-1.062-1.06a.75.75 0 1 1 1.061-1.061l1.06 1.06a.75.75 0 0 1 .001 1.06zM4.464 4.464a.75.75 0 0 1-1.06 0L2.343 3.404a.75.75 0 0 1 1.06-1.061l1.061 1.06a.75.75 0 0 1 0 1.061z"/></svg>`),
		"moon": trustedSVG(`<svg xmlns="http://www.w3.org/2000/svg" ` + cls +
			`><path d="M9.598 1.591a.75.75 0 0 1 .785-.175 7 7 0 1 1-8.967 8.967.75.75 0 0 1 .961-.96 5.5 5.5 0 0 0 7.046-7.046.75.75 0 0 1 .175-.786z"/></svg>`),
		"alert": trustedSVG(`<svg xmlns="http://www.w3.org/2000/svg" ` + cls +
			`><path d="M6.457 1.047c.659-1.234 2.427-1.234 3.086 0l6.082 11.378A1.75 1.75 0 0 1 14.082 15H1.918a1.75 1.75 0 0 1-1.543-2.575zM8 5a.75.75 0 0 0-.75.75v2.5a.75.75 0 0 0 1.5 0v-2.5A.75.75 0 0 0 8 5zm1 6a1 1 0 1 1-2 0 1 1 0 0 1 2 0z"/></svg>`),
	}
	return func(name string) (template.HTML, bool) {
		v, ok := icons[name]
		return v, ok
	}
}
