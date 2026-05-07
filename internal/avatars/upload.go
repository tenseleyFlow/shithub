// SPDX-License-Identifier: AGPL-3.0-or-later

package avatars

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"

	// Side-effect imports register decoders for jpeg/gif inputs.
	_ "image/gif"
	_ "image/jpeg"

	"golang.org/x/image/draw"
)

// MaxUploadBytes caps how big an uploaded avatar may be before decoding.
// Stops trivially large uploads from soaking RAM during decode.
const MaxUploadBytes = 5 * 1024 * 1024 // 5 MiB

// MaxPixelArea bounds the *decoded* image's pixel area as a defense-in-depth
// check against decompression-bomb attacks: a small file (well under
// MaxUploadBytes) can decode to a huge image. We reject anything past 24
// megapixels (e.g. 4900×4900), well above any reasonable avatar source.
const MaxPixelArea = 24 * 1000 * 1000

// VariantSizes is the set of resize targets we generate per upload. The
// largest is what the public avatar route serves; smaller ones are kept
// so a future "/avatars/:user/:size" route can serve them without
// re-resizing.
var VariantSizes = []int{460, 200, 40}

// Variant is one rendered output: a sized PNG ready for upload to the
// object store.
type Variant struct {
	Size int    // edge length in pixels (Size × Size)
	Data []byte // PNG bytes
}

// Errors surfaced to the handler. Each maps to a friendly UI message.
var (
	ErrTooLarge      = errors.New("avatars: upload exceeds size limit")
	ErrUnsupported   = errors.New("avatars: unsupported image format")
	ErrDecompression = errors.New("avatars: image dimensions exceed limit")
	ErrDecode        = errors.New("avatars: could not decode image")
)

// Process reads an uploaded image, validates it, and produces resized
// PNG variants. It strips EXIF as a side effect of re-encoding.
//
// Returns the variants in VariantSizes order plus a content-addressed key
// component (sha256 of the largest variant's bytes) the caller can embed
// in the storage path.
func Process(r io.Reader) ([]Variant, string, error) {
	// Bound the read up-front; one extra byte to detect overflow.
	limited := io.LimitReader(r, MaxUploadBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("read upload: %w", err)
	}
	if int64(len(raw)) > MaxUploadBytes {
		return nil, "", ErrTooLarge
	}

	// Decode metadata first so we can reject decompression bombs without
	// allocating the full pixel buffer.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrDecode
	}
	if !isSupportedFormat(format) {
		return nil, "", ErrUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixelArea {
		return nil, "", ErrDecompression
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrDecode
	}

	// Square-crop to the shorter side so the resize doesn't squash.
	cropped := centerSquareCrop(src)

	out := make([]Variant, 0, len(VariantSizes))
	for _, size := range VariantSizes {
		dst := image.NewRGBA(image.Rect(0, 0, size, size))
		draw.CatmullRom.Scale(dst, dst.Bounds(), cropped, cropped.Bounds(), draw.Over, nil)
		buf := &bytes.Buffer{}
		if err := png.Encode(buf, dst); err != nil {
			return nil, "", fmt.Errorf("encode %dpx: %w", size, err)
		}
		out = append(out, Variant{Size: size, Data: buf.Bytes()})
	}

	// Content-addressed key from the largest (first) variant. Avatar URLs
	// embed this hash so caches invalidate when the image changes.
	digest := sha256.Sum256(out[0].Data)
	return out, hex.EncodeToString(digest[:])[:16], nil
}

// isSupportedFormat whitelists the formats we'll accept. PNG, JPEG, GIF
// are the GitHub-set; we re-encode all to PNG so output is uniform.
func isSupportedFormat(format string) bool {
	switch strings.ToLower(format) {
	case "png", "jpeg", "gif":
		return true
	}
	return false
}

// centerSquareCrop returns a centered square sub-image of src. If src is
// already square, returns src unchanged.
func centerSquareCrop(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == h {
		return src
	}
	side := w
	if h < side {
		side = h
	}
	x0 := b.Min.X + (w-side)/2
	y0 := b.Min.Y + (h-side)/2
	rect := image.Rect(x0, y0, x0+side, y0+side)
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := src.(subImager); ok {
		return si.SubImage(rect)
	}
	// Fallback: copy the cropped region.
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Copy(dst, image.Point{}, src, rect, draw.Src, nil)
	return dst
}
