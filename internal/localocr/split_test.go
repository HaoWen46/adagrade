package localocr

import (
	"image"
	"image/color"
	"testing"
)

// fillRect paints a solid color rectangle into img.
func fillRect(img *image.Gray, r image.Rectangle, c color.Gray) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetGray(x, y, c)
		}
	}
}

// whiteGray returns an all-white grayscale image of the given size.
func whiteGray(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	fillRect(img, img.Bounds(), color.Gray{Y: 255})
	return img
}

func TestSplitLines_TwoBands(t *testing.T) {
	// 100x100 white canvas with two horizontal black bars (text lines):
	// rows 10..25 and rows 60..80. Expect two bands roughly bracketing them.
	img := whiteGray(100, 100)
	black := color.Gray{Y: 0}
	fillRect(img, image.Rect(20, 10, 80, 25), black)
	fillRect(img, image.Rect(20, 60, 80, 80), black)

	bands := splitLines(img)
	if len(bands) != 2 {
		t.Fatalf("want 2 bands, got %d: %+v", len(bands), bands)
	}
	// First band must cover the first bar's ink rows (with padding it can
	// extend a little, but must include 10..25 and not spill past the gap).
	b0, b1 := bands[0], bands[1]
	if b0.Min.Y > 10 || b0.Max.Y < 25 {
		t.Errorf("band0 %v does not bracket ink rows 10..25", b0)
	}
	if b1.Min.Y > 60 || b1.Max.Y < 80 {
		t.Errorf("band1 %v does not bracket ink rows 60..80", b1)
	}
	if b0.Max.Y >= b1.Min.Y {
		t.Errorf("bands overlap/misordered: %v then %v", b0, b1)
	}
	// Left/right trim: ink only spans x=20..80, so bands should be trimmed
	// away from the full 0..100 width.
	if b0.Min.X < 5 || b0.Max.X > 95 {
		t.Errorf("band0 x-range not trimmed to ink: %v", b0)
	}
}

func TestSplitLines_SingleLine(t *testing.T) {
	img := whiteGray(120, 40)
	fillRect(img, image.Rect(10, 12, 110, 28), color.Gray{Y: 0})
	bands := splitLines(img)
	if len(bands) != 1 {
		t.Fatalf("want 1 band, got %d: %+v", len(bands), bands)
	}
	if bands[0].Min.Y > 12 || bands[0].Max.Y < 28 {
		t.Errorf("band %v does not bracket ink rows 12..28", bands[0])
	}
}

func TestSplitLines_EmptyFallsBackToWholeCrop(t *testing.T) {
	img := whiteGray(80, 50)
	bands := splitLines(img)
	if len(bands) != 1 {
		t.Fatalf("empty image should yield 1 whole-crop band, got %d", len(bands))
	}
	if !bands[0].Eq(img.Bounds()) {
		t.Errorf("fallback band should equal full bounds %v, got %v", img.Bounds(), bands[0])
	}
}

func TestSplitLines_MergesTinyGap(t *testing.T) {
	// Two bars separated by a 1px gap should merge into a single band
	// (descenders / anti-aliasing shouldn't shatter one line into two).
	img := whiteGray(100, 60)
	black := color.Gray{Y: 0}
	fillRect(img, image.Rect(10, 20, 90, 29), black)
	fillRect(img, image.Rect(10, 30, 90, 39), black) // 1px gap at row 29
	bands := splitLines(img)
	if len(bands) != 1 {
		t.Fatalf("tiny gap should merge into 1 band, got %d: %+v", len(bands), bands)
	}
}
