package localocr

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func TestPreprocess_TensorDimsNCHW(t *testing.T) {
	// A 100x20 image, target height 48. Aspect-preserved width =
	// round(48 * 100/20) = 240. Layout NCHW => shape [1,3,48,240].
	img := whiteGray(100, 20)
	data, shape := preprocess(img, 48, 640, 0)
	want := []int64{1, 3, 48, 240}
	if len(shape) != 4 {
		t.Fatalf("shape rank: got %v", shape)
	}
	for i := range want {
		if shape[i] != want[i] {
			t.Fatalf("shape: got %v want %v", shape, want)
		}
	}
	if len(data) != 1*3*48*240 {
		t.Fatalf("data len: got %d want %d", len(data), 3*48*240)
	}
}

func TestPreprocess_WidthCap(t *testing.T) {
	// Very wide image must be capped at maxW.
	img := whiteGray(10000, 20)
	_, shape := preprocess(img, 48, 640, 0)
	if shape[3] != 640 {
		t.Fatalf("width should cap at 640, got %d", shape[3])
	}
}

func TestPreprocess_WidthFloorPad(t *testing.T) {
	// Narrow image below the floor should be padded up to minW.
	img := whiteGray(5, 48)
	_, shape := preprocess(img, 48, 640, 32)
	if shape[3] != 32 {
		t.Fatalf("width should be padded to floor 32, got %d", shape[3])
	}
}

func TestPreprocess_NormalizationWhiteAndBlack(t *testing.T) {
	// Normalization is (x/255 - 0.5)/0.5 => white(255) -> +1, black(0) -> -1.
	// Build a half-white/half-black image and spot-check both extremes exist.
	img := image.NewGray(image.Rect(0, 0, 48, 48))
	fillRect(img, image.Rect(0, 0, 24, 48), color.Gray{Y: 255})
	fillRect(img, image.Rect(24, 0, 48, 48), color.Gray{Y: 0})
	data, _ := preprocess(img, 48, 640, 0)
	var sawPlus1, sawMinus1 bool
	for _, v := range data {
		if math.Abs(float64(v)-1.0) < 1e-4 {
			sawPlus1 = true
		}
		if math.Abs(float64(v)+1.0) < 1e-4 {
			sawMinus1 = true
		}
	}
	if !sawPlus1 || !sawMinus1 {
		t.Fatalf("normalization off: white->+1 seen=%v black->-1 seen=%v", sawPlus1, sawMinus1)
	}
}

func TestPreprocess_PaddingIsWhiteNotZero(t *testing.T) {
	// A narrow image padded to floor must pad with WHITE (normalized +1),
	// matching a light background — never raw zeros (which would normalize to
	// -0.5 grey and confuse the recognizer). All-white 5px image padded to 32
	// should be entirely +1.
	img := whiteGray(5, 48)
	data, shape := preprocess(img, 48, 640, 32)
	if shape[3] != 32 {
		t.Fatalf("precondition: width %d", shape[3])
	}
	for i, v := range data {
		if math.Abs(float64(v)-1.0) > 1e-4 {
			t.Fatalf("padded pixel %d = %v, want +1 (white)", i, v)
		}
	}
}
