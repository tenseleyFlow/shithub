// SPDX-License-Identifier: AGPL-3.0-or-later

package statuspage

import (
	"encoding/xml"
	"fmt"
)

// Badge color palette — matches shields.io conventions so the badge
// fits next to existing CI-status badges visually.
const (
	colorBackground = "#555"
	colorOK         = "#4c1"    // bright green
	colorDegraded   = "#dfb317" // amber
	colorDown       = "#e05d44" // red
	colorUnknown    = "#9f9f9f" // grey
	colorPaid       = "#007ec6" // blue — the 402 "Pro feature" badge
)

// BuildBadge returns an SVG byte slice for the given state. The
// label is the left half ("status"); the right half text comes from
// the state itself ("ok", "degraded", "down", "unknown").
//
// The SVG is laid out shields.io-style: two flat rectangles with a
// drop-shadow gradient. Tight fixed widths keep us out of font-
// measurement territory — production shields.io measures glyphs
// to pick widths, but a fixed wide-enough box renders fine at the
// trade-off of a little extra horizontal padding.
func BuildBadge(state State) []byte {
	return BuildBadgeWithLabel("status", state)
}

// BuildBadgeWithLabel lets callers swap the left label (e.g.
// "shithub" for a more branded badge). Right text is derived from
// the state.
func BuildBadgeWithLabel(label string, state State) []byte {
	rightText, color := badgeTextAndColor(state)
	return renderBadge(label, rightText, color)
}

// BuildPaidBadge returns the SVG for the 402-style "Pro feature"
// teaser shown to Free users at /<username>.status.svg. The right
// text says "Pro feature" in the blue brand color.
func BuildPaidBadge() []byte {
	return renderBadge("status", "Pro feature", colorPaid)
}

func badgeTextAndColor(state State) (string, string) {
	switch state {
	case StateOK:
		return "ok", colorOK
	case StateDegraded:
		return "degraded", colorDegraded
	case StateDown:
		return "down", colorDown
	default:
		return "unknown", colorUnknown
	}
}

// svgBadge is the XML root we marshal. Defined as a typed struct so
// encoding/xml escapes every text node — defends against a malicious
// label string smuggling SVG.
type svgBadge struct {
	XMLName xml.Name `xml:"svg"`
	XMLNS   string   `xml:"xmlns,attr"`
	Width   int      `xml:"width,attr"`
	Height  int      `xml:"height,attr"`
	Body    []byte   `xml:",innerxml"`
}

const badgeFontFamily = `DejaVu Sans,Verdana,Geneva,sans-serif`

func renderBadge(left, right, rightColor string) []byte {
	// Coarse glyph-width heuristic: 7 px per char + 12 px padding
	// each side. Cuts the math without a real font shaper. Tunes
	// fine for the small label set we render.
	leftW := 12 + 7*len(left) + 6
	rightW := 12 + 7*len(right) + 6
	totalW := leftW + rightW

	// The inner XML is hand-built so we control whitespace + retain
	// shields-style appearance. We DO XML-escape the variable strings
	// to defend against caller-controlled labels.
	leftEsc := xmlEscape(left)
	rightEsc := xmlEscape(right)
	bg := xmlEscape(colorBackground)
	fg := xmlEscape(rightColor)

	inner := []byte(fmt.Sprintf(
		`<linearGradient id="g" x2="0" y2="100%%">`+
			`<stop offset="0" stop-color="#bbb" stop-opacity=".1"/>`+
			`<stop offset="1" stop-opacity=".1"/></linearGradient>`+
			`<rect width="%d" height="20" fill="%s"/>`+
			`<rect x="%d" width="%d" height="20" fill="%s"/>`+
			`<rect width="%d" height="20" fill="url(#g)"/>`+
			`<g fill="#fff" text-anchor="middle" font-family="%s" font-size="11">`+
			`<text x="%d" y="14">%s</text>`+
			`<text x="%d" y="14">%s</text></g>`,
		leftW, bg,
		leftW, rightW, fg,
		totalW,
		badgeFontFamily,
		leftW/2, leftEsc,
		leftW+rightW/2, rightEsc,
	))

	b := svgBadge{
		XMLNS:  "http://www.w3.org/2000/svg",
		Width:  totalW,
		Height: 20,
		Body:   inner,
	}
	out, _ := xml.Marshal(&b)
	// Prepend XML decl so generic SVG consumers (some markdown
	// renderers) parse cleanly.
	return append([]byte(`<?xml version="1.0" encoding="UTF-8"?>`), out...)
}

// xmlEscape replaces the five XML metacharacters. Manual rather than
// encoding/xml.Escape because we're building innerxml by string
// concat and need to escape only the text portions.
func xmlEscape(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '&' || c == '<' || c == '>' || c == '"' || c == '\'' || (c < 0x20 && c != '\t' && c != '\n' && c != '\r') {
			return xmlEscapeSlow(s)
		}
	}
	return s
}

func xmlEscapeSlow(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			out = append(out, "&amp;"...)
		case '<':
			out = append(out, "&lt;"...)
		case '>':
			out = append(out, "&gt;"...)
		case '"':
			out = append(out, "&quot;"...)
		case '\'':
			out = append(out, "&#39;"...)
		default:
			if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
				continue // drop ASCII control chars
			}
			out = append(out, c)
		}
	}
	return string(out)
}
