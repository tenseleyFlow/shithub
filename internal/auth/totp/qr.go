// SPDX-License-Identifier: AGPL-3.0-or-later

package totp

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// QRSize is the rendered SVG side length in CSS pixels (the SVG itself
// is resolution-independent; this drives the viewport-mapped size).
const QRSize = 256

// QRSVG renders the otpauth URI as an SVG QR code suitable for inline
// embedding in a template. Returns template.HTML so html/template doesn't
// double-escape the markup.
//
// The URI is high-entropy and contains the secret. Callers MUST NOT log
// either the URI or the rendered SVG.
func QRSVG(otpauthURI string) (template.HTML, error) {
	code, err := qr.Encode(otpauthURI, qr.M, qr.Auto)
	if err != nil {
		return "", fmt.Errorf("totp: qr encode: %w", err)
	}
	bounds := code.Bounds()
	side := bounds.Dx()
	if side <= 0 {
		return "", fmt.Errorf("totp: qr empty bounds")
	}

	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="2FA setup QR code">`,
		QRSize, QRSize, side, side)
	// White background.
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, side, side)

	// Emit one <rect> per module. For typical otpauth URIs the QR is
	// ~33×33 modules, so the SVG stays under a few KB.
	type img interface {
		Get(x, y int) bool
	}
	g, ok := code.(img)
	if !ok {
		return "", fmt.Errorf("totp: qr type lacks Get(x,y) accessor")
	}
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			if g.Get(x, y) {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1" fill="#000000"/>`, x, y)
			}
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), nil //nolint:gosec // we built the SVG ourselves; no untrusted input.
}
