// SPDX-License-Identifier: AGPL-3.0-or-later

package avatars

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"io"
	"strings"

	// Side-effect import registers the jpeg decoder.
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

// Variant is one rendered output ready for upload to the object store.
// PRO-EXT01-04b added ContentType and Ext so an animated-GIF preserve
// path can co-exist with the static-PNG variants without forcing all
// callers to special-case the canonical key extension.
type Variant struct {
	Size        int    // edge length in pixels (Size × Size)
	Data        []byte // encoded image bytes
	ContentType string // MIME type for the object store + serve handler
	Ext         string // file extension (no leading dot)
}

// Options controls the per-upload behavior of Process. The zero value
// is the legacy static-PNG path.
type Options struct {
	// AllowAnimated, when true, preserves the raw bytes of a
	// multi-frame GIF upload as the canonical variant (served at its
	// native size with image/gif Content-Type). Single-frame GIFs and
	// non-GIF uploads still flow through the PNG resize path. False
	// keeps the legacy flatten-to-PNG behavior for all inputs.
	AllowAnimated bool
}

// Errors surfaced to the handler. Each maps to a friendly UI message.
var (
	ErrTooLarge      = errors.New("avatars: upload exceeds size limit")
	ErrUnsupported   = errors.New("avatars: unsupported image format")
	ErrDecompression = errors.New("avatars: image dimensions exceed limit")
	ErrDecode        = errors.New("avatars: could not decode image")
)

// Process is the legacy entrypoint preserved for callers that don't
// participate in the animated-avatar entitlement (e.g. org avatars).
// New callers should use ProcessOpts to opt into Pro-tier behavior.
func Process(r io.Reader) ([]Variant, string, error) {
	return ProcessOpts(r, Options{})
}

// ProcessOpts reads an uploaded image, validates it, and produces resized
// PNG variants. It strips EXIF as a side effect of re-encoding.
//
// When opts.AllowAnimated is true and the upload is a multi-frame GIF,
// the canonical (largest) variant is the original raw GIF bytes with
// image/gif content type; smaller variants are still produced as static
// PNG thumbnails (the first frame, square-cropped). Multi-frame GIFs
// without AllowAnimated get flattened to all-PNG as before.
//
// Returns the variants in VariantSizes order plus a content-addressed key
// component (sha256 of the largest variant's bytes) the caller can embed
// in the storage path.
func ProcessOpts(r io.Reader, opts Options) ([]Variant, string, error) {
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

	// Pro-tier animated-GIF preserve path: keep the raw bytes for the
	// canonical variant so the public avatar endpoint can serve the
	// animated image untouched. Thumbnails still go through PNG resize
	// so list views remain cheap.
	if opts.AllowAnimated && format == "gif" {
		animated, src, err := decodeGIFAnimated(raw)
		if err != nil {
			return nil, "", err
		}
		if animated {
			out, hash := assembleAnimatedGIF(raw, src)
			return out, hash, nil
		}
		// Single-frame GIF: fall through to the static PNG path so the
		// stored object stays a normal PNG (cheaper to serve and no
		// behavior difference for the user).
		cropped := centerSquareCrop(src)
		return assembleStaticPNG(cropped)
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrDecode
	}
	// Square-crop to the shorter side so the resize doesn't squash.
	cropped := centerSquareCrop(src)
	out, hash, err := assembleStaticPNG(cropped)
	if err != nil {
		return nil, "", err
	}
	return out, hash, nil
}

// assembleStaticPNG resizes a cropped source image into the configured
// VariantSizes set of PNG variants. Returns the same shape Process has
// always returned so legacy callers don't observe a change.
func assembleStaticPNG(cropped image.Image) ([]Variant, string, error) {
	out := make([]Variant, 0, len(VariantSizes))
	for _, size := range VariantSizes {
		dst := image.NewRGBA(image.Rect(0, 0, size, size))
		draw.CatmullRom.Scale(dst, dst.Bounds(), cropped, cropped.Bounds(), draw.Over, nil)
		buf := &bytes.Buffer{}
		if err := png.Encode(buf, dst); err != nil {
			return nil, "", fmt.Errorf("encode %dpx: %w", size, err)
		}
		out = append(out, Variant{
			Size:        size,
			Data:        buf.Bytes(),
			ContentType: "image/png",
			Ext:         "png",
		})
	}
	digest := sha256.Sum256(out[0].Data)
	return out, hex.EncodeToString(digest[:])[:16], nil
}

// assembleAnimatedGIF produces the variant list for a Pro-tier animated
// upload: canonical variant is the original GIF bytes (so animation
// survives); thumbnails are static PNGs derived from the first frame so
// list/comment renders don't pay the cost of decoding an animation per
// row. The content-addressed hash uses the raw GIF bytes so the URL
// changes on every upload even when the first frame happens to look
// identical to a previous one.
func assembleAnimatedGIF(raw []byte, firstFrame image.Image) ([]Variant, string) {
	cropped := centerSquareCrop(firstFrame)
	out := make([]Variant, 0, len(VariantSizes))
	canonical := VariantSizes[0]
	for _, size := range VariantSizes {
		if size == canonical {
			out = append(out, Variant{
				Size:        size,
				Data:        raw,
				ContentType: "image/gif",
				Ext:         "gif",
			})
			continue
		}
		dst := image.NewRGBA(image.Rect(0, 0, size, size))
		draw.CatmullRom.Scale(dst, dst.Bounds(), cropped, cropped.Bounds(), draw.Over, nil)
		buf := &bytes.Buffer{}
		if err := png.Encode(buf, dst); err == nil {
			out = append(out, Variant{
				Size:        size,
				Data:        buf.Bytes(),
				ContentType: "image/png",
				Ext:         "png",
			})
		}
	}
	digest := sha256.Sum256(raw)
	return out, hex.EncodeToString(digest[:])[:16]
}

// decodeGIFAnimated reports whether raw is a multi-frame GIF and
// returns the first frame as the static fallback. The first frame is
// reused for thumbnails so we don't decode twice.
func decodeGIFAnimated(raw []byte) (animated bool, firstFrame image.Image, err error) {
	all, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		return false, nil, ErrDecode
	}
	if len(all.Image) == 0 {
		return false, nil, ErrDecode
	}
	return len(all.Image) > 1, all.Image[0], nil
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
