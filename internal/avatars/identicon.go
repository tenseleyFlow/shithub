// SPDX-License-Identifier: AGPL-3.0-or-later

// Package avatars owns avatar resolution for shithub user profiles. S09
// ships the deterministic SVG identicon (rendered server-side, no upload
// needed) plus the routing skeleton for serving uploaded avatars from
// object storage. Real upload (resize / EXIF-strip) lands in S10.
package avatars

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// IdenticonSize is the SVG viewBox side length. The image is square and
// renders at any CSS px size via width/height attributes.
const IdenticonSize = 5

// palette is the fixed accent set used to color identicons. Sourced from
// Primer's accent ramp so the result feels native to the rest of the
// shithub chrome. 12 entries gives reasonable variety without
// overwhelming the page.
var palette = []string{
	"#e85aad", // pink
	"#bf3989", // pink-deep
	"#a371f7", // purple
	"#8957e5", // purple-deep
	"#388bfd", // blue
	"#1f6feb", // blue-deep
	"#3fb950", // green
	"#238636", // green-deep
	"#d29922", // yellow-deep
	"#db6d28", // orange
	"#bd561d", // orange-deep
	"#f85149", // red
}

// Identicon returns an inline SVG identicon for username. Same input →
// same output (deterministic). The SVG carries width/height attributes
// so it can be embedded as inline markup OR served as a standalone file.
//
// The pattern is a 5×5 grid mirrored horizontally — left two columns
// are mirrored to the right, the middle column is independent. Each
// cell is colored if the corresponding bit in the digest is 1.
func Identicon(username string, pixelSize int) string {
	if pixelSize <= 0 {
		pixelSize = 80
	}
	digest := sha256.Sum256([]byte(strings.ToLower(username)))
	color := palette[int(digest[0])%len(palette)]

	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="identicon">`,
		pixelSize, pixelSize, IdenticonSize, IdenticonSize)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#21262d"/>`, IdenticonSize, IdenticonSize)

	// Walk 15 cells (5 rows × 3 unique columns). Mirror the first two
	// columns onto the last two so the identicon is symmetric.
	bit := 0
	for row := 0; row < IdenticonSize; row++ {
		for col := 0; col < 3; col++ {
			b8 := digest[1+(bit/8)] // bytes 1..15 cover the 25 bits we sample
			on := (b8 >> (uint(bit) % 8)) & 1
			bit++
			if on != 1 {
				continue
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1" fill="%s"/>`, col, row, color)
			if col < 2 {
				mirror := IdenticonSize - 1 - col
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1" fill="%s"/>`, mirror, row, color)
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}
