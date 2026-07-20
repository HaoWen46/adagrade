package ingest

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: 200, B: uint8(y % 255), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNormalizeImage_DecodesPNGAndDownscales(t *testing.T) {
	data := pngBytes(t, 4000, 2000) // 2:1, long edge 4000
	pg, err := NormalizeImage(data, render.Options{MaxLongEdgePx: 1000, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	if pg.Width != 1000 || pg.Height != 500 {
		t.Errorf("dimensions: got %dx%d want 1000x500 (aspect preserved)", pg.Width, pg.Height)
	}
	if pg.SHA256 == "" {
		t.Error("missing sha256")
	}
	// Output must decode as JPEG.
	if _, err := jpeg.Decode(bytes.NewReader(pg.JPEG)); err != nil {
		t.Errorf("output is not valid JPEG: %v", err)
	}
}

func TestNormalizeImage_DecodesJPEGNoUpscale(t *testing.T) {
	data := jpegBytes(t, 300, 200) // long edge already under the cap
	pg, err := NormalizeImage(data, render.Options{MaxLongEdgePx: 2200, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	if pg.Width != 300 || pg.Height != 200 {
		t.Errorf("should not upscale: got %dx%d want 300x200", pg.Width, pg.Height)
	}
}

func TestNormalizeImage_RejectsGarbage(t *testing.T) {
	if _, err := NormalizeImage([]byte("not an image"), render.Options{}); err == nil {
		t.Fatal("expected error decoding garbage bytes")
	}
}

func TestSniffImageExt(t *testing.T) {
	if ext := sniffImageExt(pngBytes(t, 10, 10)); ext != "png" {
		t.Errorf("png sniff: got %q want png", ext)
	}
	if ext := sniffImageExt(jpegBytes(t, 10, 10)); ext != "jpg" {
		t.Errorf("jpeg sniff: got %q want jpg", ext)
	}
}
