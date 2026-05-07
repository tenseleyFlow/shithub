// SPDX-License-Identifier: AGPL-3.0-or-later

package avatars_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/avatars"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	buf := &bytes.Buffer{}
	if err := png.Encode(buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	buf := &bytes.Buffer{}
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func TestProcess_PNGRoundtrip(t *testing.T) {
	t.Parallel()
	src := makePNG(t, 800, 600) // landscape; expect center-crop to 600×600.
	variants, hash, err := avatars.Process(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(variants) != len(avatars.VariantSizes) {
		t.Fatalf("variants = %d, want %d", len(variants), len(avatars.VariantSizes))
	}
	for i, want := range avatars.VariantSizes {
		v := variants[i]
		if v.Size != want {
			t.Errorf("variant[%d].Size = %d, want %d", i, v.Size, want)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(v.Data))
		if err != nil {
			t.Errorf("variant[%d] decode: %v", i, err)
			continue
		}
		if cfg.Width != want || cfg.Height != want {
			t.Errorf("variant[%d] dims = %dx%d, want %dx%d", i, cfg.Width, cfg.Height, want, want)
		}
	}
	if len(hash) == 0 {
		t.Fatal("hash is empty")
	}
}

func TestProcess_JPEGAccepted(t *testing.T) {
	t.Parallel()
	src := makeJPEG(t, 1000, 1000)
	if _, _, err := avatars.Process(bytes.NewReader(src)); err != nil {
		t.Fatalf("jpeg accepted: %v", err)
	}
}

func TestProcess_RejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	_, _, err := avatars.Process(strings.NewReader("not an image at all"))
	if err == nil {
		t.Fatal("expected error for non-image input")
	}
}

func TestProcess_RejectsTooLarge(t *testing.T) {
	t.Parallel()
	// Build a payload that exceeds MaxUploadBytes.
	big := make([]byte, avatars.MaxUploadBytes+10)
	_, _, err := avatars.Process(bytes.NewReader(big))
	if err != avatars.ErrTooLarge {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestProcess_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()
	// Construct a tiny PNG header that *claims* huge dimensions but
	// stays well under MaxUploadBytes.
	// 10000 × 10000 = 100M pixels > 24M cap.
	// Build a real but huge-size PNG using NewPaletted to keep file size low.
	pal := []color.Color{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, 8000, 8000), pal)
	buf := &bytes.Buffer{}
	if err := png.Encode(buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() > avatars.MaxUploadBytes {
		t.Skipf("constructed PNG (%d bytes) exceeds MaxUploadBytes; skipping", buf.Len())
	}
	_, _, err := avatars.Process(bytes.NewReader(buf.Bytes()))
	if err != avatars.ErrDecompression {
		t.Fatalf("err = %v, want ErrDecompression", err)
	}
}
