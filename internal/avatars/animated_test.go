// SPDX-License-Identifier: AGPL-3.0-or-later

package avatars_test

// PRO-EXT01-04b: covers the animated-GIF preserve branch. The static
// tests in upload_test.go already cover the legacy PNG path; here we
// pin the new behavior so a refactor that drops AllowAnimated handling
// can't silently flatten Pro uploads.

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/avatars"
)

func makeAnimatedGIF(t *testing.T, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{R: 0xff}, color.RGBA{B: 0xff}, color.Transparent}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < frames; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 64, 64), pal)
		// Fill with alternating colors so the frames are not identical.
		fill := uint8(i % 2)
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				img.SetColorIndex(x, y, fill)
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	buf := &bytes.Buffer{}
	if err := gif.EncodeAll(buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// TestProcessOpts_AnimatedGIFPreservesRawBytes asserts the canonical
// variant is byte-identical to the input GIF when the entitlement
// permits animation. Anything else (a frame-decoded re-encode) would
// silently strip metadata Pro users paid to keep.
func TestProcessOpts_AnimatedGIFPreservesRawBytes(t *testing.T) {
	t.Parallel()
	src := makeAnimatedGIF(t, 3)
	variants, hash, err := avatars.ProcessOpts(bytes.NewReader(src), avatars.Options{AllowAnimated: true})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if hash == "" {
		t.Fatal("hash empty")
	}
	if len(variants) == 0 {
		t.Fatal("no variants")
	}
	canonical := variants[0]
	if canonical.ContentType != "image/gif" {
		t.Errorf("canonical ContentType = %q, want image/gif", canonical.ContentType)
	}
	if canonical.Ext != "gif" {
		t.Errorf("canonical Ext = %q, want gif", canonical.Ext)
	}
	if !bytes.Equal(canonical.Data, src) {
		t.Errorf("canonical Data was modified — Pro path must preserve raw GIF bytes")
	}
	// Confirm it still decodes as an animation with the same frame count.
	got, err := gif.DecodeAll(bytes.NewReader(canonical.Data))
	if err != nil {
		t.Fatalf("re-decode canonical: %v", err)
	}
	if len(got.Image) != 3 {
		t.Errorf("frame count = %d, want 3", len(got.Image))
	}
}

// TestProcessOpts_AnimatedGIFThumbnailsAreStaticPNG: the smaller
// variants are PNG flatten of the first frame. Critical for list/avatar
// renders where a 40px animated GIF would be silly.
func TestProcessOpts_AnimatedGIFThumbnailsAreStaticPNG(t *testing.T) {
	t.Parallel()
	src := makeAnimatedGIF(t, 4)
	variants, _, err := avatars.ProcessOpts(bytes.NewReader(src), avatars.Options{AllowAnimated: true})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(variants) < 2 {
		t.Fatalf("expected animated path to still produce thumbnails; got %d variants", len(variants))
	}
	for _, v := range variants[1:] {
		if v.ContentType != "image/png" {
			t.Errorf("thumbnail size=%d ContentType = %q, want image/png", v.Size, v.ContentType)
		}
		if _, err := png.DecodeConfig(bytes.NewReader(v.Data)); err != nil {
			t.Errorf("thumbnail size=%d not valid PNG: %v", v.Size, err)
		}
	}
}

// TestProcessOpts_AnimatedGIFFlattensWhenDisallowed pins the Free path:
// even when the upload is a multi-frame GIF, the result is the legacy
// all-PNG static set. Anything else and a Free user would get the Pro
// benefit during the enforce window.
func TestProcessOpts_AnimatedGIFFlattensWhenDisallowed(t *testing.T) {
	t.Parallel()
	src := makeAnimatedGIF(t, 3)
	variants, _, err := avatars.ProcessOpts(bytes.NewReader(src), avatars.Options{AllowAnimated: false})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	for _, v := range variants {
		if v.ContentType != "image/png" {
			t.Errorf("size=%d ContentType = %q, want image/png (Free should always flatten)", v.Size, v.ContentType)
		}
	}
}

// TestProcessOpts_SingleFrameGIFFlattensEvenWithAllowAnimated: a
// single-frame GIF has nothing to animate. We deliberately drop into
// the cheap PNG path so the stored object isn't a needlessly larger GIF.
func TestProcessOpts_SingleFrameGIFFlattensEvenWithAllowAnimated(t *testing.T) {
	t.Parallel()
	src := makeAnimatedGIF(t, 1)
	variants, _, err := avatars.ProcessOpts(bytes.NewReader(src), avatars.Options{AllowAnimated: true})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	for _, v := range variants {
		if v.ContentType != "image/png" {
			t.Errorf("single-frame GIF size=%d ContentType = %q, want image/png", v.Size, v.ContentType)
		}
	}
}

// TestProcessOpts_PNGUnchangedByAllowAnimated confirms the new option
// doesn't alter the PNG path — important because the org-avatar
// handler still calls plain Process (legacy entry) and shares the
// underlying pipeline.
func TestProcessOpts_PNGUnchangedByAllowAnimated(t *testing.T) {
	t.Parallel()
	src := makePNG(t, 500, 500)
	variants, _, err := avatars.ProcessOpts(bytes.NewReader(src), avatars.Options{AllowAnimated: true})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	for _, v := range variants {
		if v.ContentType != "image/png" {
			t.Errorf("png size=%d ContentType = %q, want image/png", v.Size, v.ContentType)
		}
	}
}
