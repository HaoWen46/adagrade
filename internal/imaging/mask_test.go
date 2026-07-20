package imaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

// ---- test-image generation (no fixtures on disk) ----

const (
	testW = 400
	testH = 300
)

// newTestJPEG builds a white 400x300 JPEG with a colored grid: 2px red
// vertical lines and 2px blue horizontal lines every 50px. Probe points in
// the tests are chosen in flat areas away from grid lines so JPEG ringing
// does not pollute the deltas.
func newTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, testW, testH))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	red := color.RGBA{R: 0xff, A: 0xff}
	blue := color.RGBA{B: 0xff, A: 0xff}
	for x := 50; x < testW; x += 50 {
		draw.Draw(img, image.Rect(x, 0, x+2, testH), image.NewUniform(red), image.Point{}, draw.Src)
	}
	for y := 50; y < testH; y += 50 {
		draw.Draw(img, image.Rect(0, y, testW, y+2), image.NewUniform(blue), image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return buf.Bytes()
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	return img
}

func rgbAt(img image.Image, x, y int) (uint8, uint8, uint8) {
	c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	return c.R, c.G, c.B
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// assertNearColor checks each channel of img at (x,y) is within tol of want.
func assertNearColor(t *testing.T, img image.Image, x, y int, want color.RGBA, tol int) {
	t.Helper()
	r, g, b := rgbAt(img, x, y)
	if absDiff(r, want.R) > tol || absDiff(g, want.G) > tol || absDiff(b, want.B) > tol {
		t.Errorf("pixel (%d,%d) = (%d,%d,%d), want ~(%d,%d,%d) within %d",
			x, y, r, g, b, want.R, want.G, want.B, tol)
	}
}

// assertNearImage checks img at (x,y) is within tol per channel of ref at (x,y).
func assertNearImage(t *testing.T, img, ref image.Image, x, y int, tol int) {
	t.Helper()
	r, g, b := rgbAt(img, x, y)
	wr, wg, wb := rgbAt(ref, x, y)
	if absDiff(r, wr) > tol || absDiff(g, wg) > tol || absDiff(b, wb) > tol {
		t.Errorf("pixel (%d,%d) = (%d,%d,%d), want ~original (%d,%d,%d) within %d",
			x, y, r, g, b, wr, wg, wb, tol)
	}
}

var defaultFillWant = color.RGBA{R: 0x4a, G: 0x4a, B: 0x4a, A: 0xff}

// Base region used across tests: pixel rect [100,200) x [75,150) on 400x300.
var baseRegion = Region{X: 0.25, Y: 0.25, W: 0.25, H: 0.25, Color: "#4a4a4a"}

// ---- Mask: fill + preservation ----

func TestMaskFillsRegionInterior(t *testing.T) {
	orig := newTestJPEG(t)
	m, err := Mask(orig, []Region{baseRegion}, 0)
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	// Probe deep inside the rect (JPEG is lossy at boundaries).
	for _, p := range [][2]int{{150, 112}, {120, 95}, {180, 130}, {150, 90}} {
		assertNearColor(t, got, p[0], p[1], defaultFillWant, 8)
	}
}

func TestMaskLeavesOutsidePixelsUnchanged(t *testing.T) {
	orig := newTestJPEG(t)
	m, err := Mask(orig, []Region{baseRegion}, 0)
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	ref := decodeJPEG(t, orig)
	// Flat white spots well outside the masked rect and away from grid lines.
	for _, p := range [][2]int{{30, 30}, {330, 280}, {170, 270}, {370, 30}} {
		assertNearImage(t, got, ref, p[0], p[1], 12)
	}
}

func TestMaskPaddingExpandsCoverage(t *testing.T) {
	orig := newTestJPEG(t)

	// (90,112) is outside the unpadded pixel rect [100,200)x[75,150)...
	unpadded, err := Mask(orig, []Region{baseRegion}, 0)
	if err != nil {
		t.Fatalf("Mask (no padding): %v", err)
	}
	ref := decodeJPEG(t, orig)
	assertNearImage(t, decodeJPEG(t, unpadded.JPEG()), ref, 90, 112, 12)

	// ...but inside the padded rect [80,220)x[60,195) when Padding=0.05.
	padded := baseRegion
	padded.Padding = 0.05
	m, err := Mask(orig, []Region{padded}, 0)
	if err != nil {
		t.Fatalf("Mask (padding): %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	assertNearColor(t, got, 90, 112, defaultFillWant, 8)
	assertNearColor(t, got, 210, 112, defaultFillWant, 8) // right padded band
}

func TestMaskOutOfBoundsRegionClampsToEdge(t *testing.T) {
	orig := newTestJPEG(t)
	// X=0.9, W=0.3 extends past the right edge: pixel rect clamps to [360,400).
	r := Region{X: 0.9, Y: 0.1, W: 0.3, H: 0.3, Color: "#4a4a4a"}
	m, err := Mask(orig, []Region{r}, 0)
	if err != nil {
		t.Fatalf("Mask with out-of-bounds region: %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	assertNearColor(t, got, 380, 75, defaultFillWant, 8)
	assertNearColor(t, got, 399, 75, defaultFillWant, 8) // covered up to the right edge
}

func TestMaskFullyOutsideRegionSkipped(t *testing.T) {
	orig := newTestJPEG(t)
	// Entirely outside the canvas: empty intersection is skipped, not an error.
	r := Region{X: 1.5, Y: 1.5, W: 0.2, H: 0.2}
	m, err := Mask(orig, []Region{r}, 0)
	if err != nil {
		t.Fatalf("Mask with fully out-of-bounds region: %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	ref := decodeJPEG(t, orig)
	assertNearImage(t, got, ref, 30, 30, 12)
}

func TestMaskMultipleRegions(t *testing.T) {
	orig := newTestJPEG(t)
	regions := []Region{
		baseRegion, // [100,200)x[75,150), gray
		// [240,320)x[210,270), red via short #rgb form.
		{X: 0.6, Y: 0.7, W: 0.2, H: 0.2, Color: "#f00"},
	}
	m, err := Mask(orig, regions, 0)
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}
	got := decodeJPEG(t, m.JPEG())
	assertNearColor(t, got, 150, 112, defaultFillWant, 8)
	assertNearColor(t, got, 280, 240, color.RGBA{R: 0xff, A: 0xff}, 8)
}

func TestMaskInvalidColorFallsBackToDefault(t *testing.T) {
	orig := newTestJPEG(t)
	for _, bad := range []string{"", "not-a-color", "#12345", "4a4a4a", "#gggggg"} {
		r := baseRegion
		r.Color = bad
		m, err := Mask(orig, []Region{r}, 0)
		if err != nil {
			t.Fatalf("Mask with color %q: %v", bad, err)
		}
		got := decodeJPEG(t, m.JPEG())
		assertNearColor(t, got, 150, 112, defaultFillWant, 8)
	}
}

// ---- Mask: encoding behavior ----

func TestMaskDeterministic(t *testing.T) {
	orig := newTestJPEG(t)
	regions := []Region{baseRegion, {X: 0.6, Y: 0.7, W: 0.2, H: 0.2, Color: "#f00"}}
	m1, err := Mask(orig, regions, 0)
	if err != nil {
		t.Fatalf("Mask #1: %v", err)
	}
	m2, err := Mask(orig, regions, 0)
	if err != nil {
		t.Fatalf("Mask #2: %v", err)
	}
	if !bytes.Equal(m1.JPEG(), m2.JPEG()) {
		t.Error("Mask is not deterministic: byte outputs differ")
	}
	if m1.SHA256() != m2.SHA256() {
		t.Errorf("SHA256 differs: %s vs %s", m1.SHA256(), m2.SHA256())
	}
	sum := sha256.Sum256(m1.JPEG())
	if want := hex.EncodeToString(sum[:]); m1.SHA256() != want {
		t.Errorf("SHA256() = %s, want sha256 of JPEG() bytes %s", m1.SHA256(), want)
	}
}

func TestMaskEmptyRegionsReencodes(t *testing.T) {
	orig := newTestJPEG(t)
	m, err := Mask(orig, nil, 0)
	if err != nil {
		t.Fatalf("Mask with no regions: %v", err)
	}
	if m.IsZero() {
		t.Error("MaskedImage from Mask should not be zero")
	}
	got := decodeJPEG(t, m.JPEG())
	if got.Bounds().Dx() != testW || got.Bounds().Dy() != testH {
		t.Errorf("re-encoded bounds = %v, want %dx%d", got.Bounds(), testW, testH)
	}
	assertNearImage(t, got, decodeJPEG(t, orig), 30, 30, 12)
}

func TestMaskInvalidInput(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("definitely not a jpeg")} {
		if _, err := Mask(in, []Region{baseRegion}, 0); err == nil {
			t.Errorf("Mask(%q) succeeded, want error", in)
		}
	}
	// PNG magic bytes must also be rejected (JPEG only).
	if _, err := Mask([]byte("\x89PNG\r\n\x1a\n0000"), nil, 0); err == nil {
		t.Error("Mask accepted PNG-prefixed bytes, want error")
	}
}

func TestMaskQualityDefaultAndExplicit(t *testing.T) {
	orig := newTestJPEG(t)
	mDefault, err := Mask(orig, nil, 0) // quality <= 0 falls back to 85
	if err != nil {
		t.Fatalf("Mask default quality: %v", err)
	}
	m85, err := Mask(orig, nil, 85)
	if err != nil {
		t.Fatalf("Mask quality 85: %v", err)
	}
	if !bytes.Equal(mDefault.JPEG(), m85.JPEG()) {
		t.Error("quality<=0 should encode identically to quality 85")
	}
}

// ---- LoadMasked: the single audited gate outside Mask (D10) ----

func TestLoadMaskedRejectsNonMaskedKey(t *testing.T) {
	b := newTestJPEG(t)
	for _, key := range []string{
		"assessments/7/pages/3/original.jpg",
		"masked", // no "/masked/" path segment
		"",
	} {
		if _, err := LoadMasked(key, b); err == nil {
			t.Errorf("LoadMasked(%q) succeeded, want error (masked-only invariant)", key)
		}
	}
}

func TestLoadMaskedRoundTrip(t *testing.T) {
	b := newTestJPEG(t)
	m, err := LoadMasked("assessments/7/masked/3.jpg", b)
	if err != nil {
		t.Fatalf("LoadMasked: %v", err)
	}
	if !bytes.Equal(m.JPEG(), b) {
		t.Error("JPEG() does not round-trip the input bytes")
	}
	sum := sha256.Sum256(b)
	if want := hex.EncodeToString(sum[:]); m.SHA256() != want {
		t.Errorf("SHA256() = %s, want %s", m.SHA256(), want)
	}
	if m.IsZero() {
		t.Error("loaded MaskedImage should not be zero")
	}
}

func TestLoadMaskedRejectsEmptyBytes(t *testing.T) {
	if _, err := LoadMasked("a/masked/1.jpg", nil); err == nil {
		t.Error("LoadMasked with empty bytes succeeded, want error")
	}
}

func TestMaskedImageIsZero(t *testing.T) {
	var zero MaskedImage
	if !zero.IsZero() {
		t.Error("zero-value MaskedImage.IsZero() = false, want true")
	}
	if zero.JPEG() != nil {
		t.Error("zero-value JPEG() should be nil")
	}
	if zero.SHA256() != "" {
		t.Error("zero-value SHA256() should be empty")
	}
}
