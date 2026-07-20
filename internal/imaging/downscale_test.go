package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

// TestDownscaleLongEdge_ShrinksToTarget asserts a large image is scaled down
// so its long edge equals maxLongEdge, aspect ratio preserved (within
// floor/ceil rounding).
func TestDownscaleLongEdge_ShrinksToTarget(t *testing.T) {
	orig := newSolidJPEG(t, 3200, 2400, color.RGBA{R: 0x20, G: 0x80, B: 0xd0, A: 0xff}, 90)
	out, err := DownscaleLongEdge(orig, 1600, 75)
	if err != nil {
		t.Fatalf("DownscaleLongEdge: %v", err)
	}
	img := decodeJPEG(t, out)
	b := img.Bounds()
	if b.Dx() != 1600 {
		t.Errorf("width = %d, want 1600 (the long edge)", b.Dx())
	}
	if b.Dy() != 1200 {
		t.Errorf("height = %d, want 1200 (aspect-preserved)", b.Dy())
	}
}

// TestDownscaleLongEdge_NoOpWhenAlreadySmaller asserts an image already under
// the cap is returned unchanged in dimensions (still re-encoded at the given
// quality — callers rely on the returned bytes being a valid JPEG at that
// quality, not byte-identical to the input).
func TestDownscaleLongEdge_NoOpWhenAlreadySmaller(t *testing.T) {
	orig := newSolidJPEG(t, 800, 600, color.RGBA{R: 0xff, A: 0xff}, 90)
	out, err := DownscaleLongEdge(orig, 1600, 75)
	if err != nil {
		t.Fatalf("DownscaleLongEdge: %v", err)
	}
	img := decodeJPEG(t, out)
	b := img.Bounds()
	if b.Dx() != 800 || b.Dy() != 600 {
		t.Errorf("dims = %dx%d, want unchanged 800x600", b.Dx(), b.Dy())
	}
}

// TestDownscaleLongEdge_PortraitOrientation asserts the long edge is
// correctly identified as height when the image is taller than wide.
func TestDownscaleLongEdge_PortraitOrientation(t *testing.T) {
	orig := newSolidJPEG(t, 1200, 1600, color.RGBA{G: 0xff, A: 0xff}, 90)
	out, err := DownscaleLongEdge(orig, 800, 75)
	if err != nil {
		t.Fatalf("DownscaleLongEdge: %v", err)
	}
	img := decodeJPEG(t, out)
	b := img.Bounds()
	if b.Dy() != 800 {
		t.Errorf("height = %d, want 800 (the long edge)", b.Dy())
	}
	if b.Dx() != 600 {
		t.Errorf("width = %d, want 600 (aspect-preserved)", b.Dx())
	}
}

// TestDownscaleLongEdge_RejectsGarbage asserts undecodable bytes error rather
// than panicking.
func TestDownscaleLongEdge_RejectsGarbage(t *testing.T) {
	if _, err := DownscaleLongEdge([]byte("not a jpeg"), 1600, 75); err == nil {
		t.Error("expected an error decoding garbage bytes")
	}
}

// TestDownscaleLongEdge_RejectsNonPositiveCap guards against a caller
// accidentally passing a zero/negative cap, which would otherwise collapse
// the image to nothing.
func TestDownscaleLongEdge_RejectsNonPositiveCap(t *testing.T) {
	orig := newSolidJPEG(t, 100, 100, color.RGBA{A: 0xff}, 90)
	if _, err := DownscaleLongEdge(orig, 0, 75); err == nil {
		t.Error("expected an error for maxLongEdge <= 0")
	}
}

func newSolidJPEG(t *testing.T, w, h int, c color.RGBA, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return buf.Bytes()
}
