package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

// ---- Crop: pixel extraction ----

// TestCropExtractsRegionPixels paints a known-color rect into the test JPEG,
// crops exactly that region (no padding), and asserts the crop's dominant
// color plus its dimensions match the region's floor/ceil pixel rect.
func TestCropExtractsRegionPixels(t *testing.T) {
	orig := newTestJPEG(t)
	// baseRegion: [100,200)x[75,150) on 400x300 — flat white interior in the
	// source, away from the grid lines, so the crop should be near-white.
	crop, err := Crop(orig, []Region{baseRegion}, 0)
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	got := decodeJPEG(t, crop.JPEG())
	wantW, wantH := 100, 75 // 200-100, 150-75
	if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
		t.Fatalf("crop bounds = %v, want %dx%d", got.Bounds(), wantW, wantH)
	}
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	// Probe well inside the crop, away from any grid-line remnants.
	for _, p := range [][2]int{{10, 10}, {40, 30}, {80, 60}} {
		assertNearColor(t, got, p[0], p[1], white, 8)
	}
}

// TestCropPaddingGrowsCropDims mirrors TestMaskPaddingExpandsCoverage: padding
// expands the cropped pixel rect on all four sides using the same
// floor/ceil ± padding math as Mask.
func TestCropPaddingGrowsCropDims(t *testing.T) {
	orig := newTestJPEG(t)
	padded := baseRegion
	padded.Padding = 0.05 // 0.05*400=20px, 0.05*300=15px on each side
	crop, err := Crop(orig, []Region{padded}, 0)
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	got := decodeJPEG(t, crop.JPEG())
	// Unpadded rect [100,200)x[75,150) grown by ~20px (x) / ~15px (y) on each
	// side via floor/ceil (mask.go's math): [80,221)x[60,165) — floating-point
	// pushes the ceil edges out by a pixel, same as Mask's padding math.
	wantW, wantH := 141, 105
	if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
		t.Fatalf("padded crop bounds = %v, want %dx%d", got.Bounds(), wantW, wantH)
	}
}

// ---- Crop: multi-region vertical stacking ----

// TestCropStacksMultipleRegionsVertically crops two differently-sized
// regions and asserts the stacked output's width is the max of the two crop
// widths and its height is the sum of the two crop heights, with each
// region's content landing in its expected vertical band.
func TestCropStacksMultipleRegionsVertically(t *testing.T) {
	orig := newTestJPEG(t)
	// Region A: [100,200)x[75,150) -> 100x75 (white interior).
	a := baseRegion
	// Region B: pixel rect [40,360)x[210,270) -> 320x60 wide red band; place
	// well away from grid lines. X=0.1,W=0.8 -> [40,360); Y=0.7,H=0.2 -> [210,270).
	b := Region{X: 0.1, Y: 0.7, W: 0.8, H: 0.2, Color: "#4a4a4a"}
	// Paint a red rectangle at exactly B's pixel rect into a fresh source
	// image so we can identify which band is which after stacking.
	painted := paintRect(t, orig, image.Rect(40, 210, 360, 270), color.RGBA{R: 0xff, A: 0xff})

	crop, err := Crop(painted, []Region{a, b}, 0)
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	got := decodeJPEG(t, crop.JPEG())

	wantW := 320 // max(100, 320)
	wantH := 75 + 60
	if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
		t.Fatalf("stacked bounds = %v, want %dx%d", got.Bounds(), wantW, wantH)
	}

	// Region A occupies the top band [0,75); it's narrower (100px) than the
	// canvas (320px), so background fill (white) should appear beside it.
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	assertNearColor(t, got, 40, 30, white, 8)   // inside A's band, inside A's width
	assertNearColor(t, got, 250, 30, white, 12) // inside A's band, in the background fill area (also white, no discriminating power alone)

	// Region B occupies the bottom band [75,135) and is red across its full
	// width, spanning the canvas width (320px == wantW), so no fill needed.
	red := color.RGBA{R: 0xff, A: 0xff}
	assertNearColor(t, got, 10, 100, red, 8)
	assertNearColor(t, got, 300, 100, red, 8)
}

// paintRect draws a solid rectangle into a copy of the given JPEG and
// re-encodes it, for tests that need to identify a region's content after
// stacking (rather than relying solely on the white background/grid).
func paintRect(t *testing.T, origJPEG []byte, rect image.Rectangle, c color.RGBA) []byte {
	t.Helper()
	src := decodeJPEG(t, origJPEG)
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	draw.Draw(dst, rect, image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode painted JPEG: %v", err)
	}
	return buf.Bytes()
}

// ---- Crop: out-of-bounds handling (a crop of nothing is a bug, unlike Mask) ----

func TestCropOutOfBoundsRegionClampedToEdge(t *testing.T) {
	orig := newTestJPEG(t)
	// X=0.9, W=0.3 extends past the right edge: pixel rect clamps to [360,400).
	r := Region{X: 0.9, Y: 0.1, W: 0.3, H: 0.3}
	crop, err := Crop(orig, []Region{r}, 0)
	if err != nil {
		t.Fatalf("Crop with out-of-bounds region: %v", err)
	}
	got := decodeJPEG(t, crop.JPEG())
	wantW := 40 // 400-360
	wantH := 90 // 0.4*300 - 0.1*300 = 120-30
	if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
		t.Fatalf("clamped crop bounds = %v, want %dx%d", got.Bounds(), wantW, wantH)
	}
}

func TestCropFullyOutsideRegionErrors(t *testing.T) {
	orig := newTestJPEG(t)
	r := Region{X: 1.5, Y: 1.5, W: 0.2, H: 0.2}
	if _, err := Crop(orig, []Region{r}, 0); err == nil {
		t.Error("Crop with fully out-of-bounds region succeeded, want error (a crop of nothing is a bug)")
	}
}

func TestCropOneOfManyFullyOutsideErrors(t *testing.T) {
	orig := newTestJPEG(t)
	regions := []Region{baseRegion, {X: 2.0, Y: 2.0, W: 0.1, H: 0.1}}
	if _, err := Crop(orig, regions, 0); err == nil {
		t.Error("Crop with one fully out-of-bounds region among several succeeded, want error")
	}
}

func TestCropEmptyRegionsErrors(t *testing.T) {
	orig := newTestJPEG(t)
	if _, err := Crop(orig, nil, 0); err == nil {
		t.Error("Crop with nil regions succeeded, want error")
	}
	if _, err := Crop(orig, []Region{}, 0); err == nil {
		t.Error("Crop with empty regions succeeded, want error")
	}
}

func TestCropInvalidInput(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("definitely not a jpeg")} {
		if _, err := Crop(in, []Region{baseRegion}, 0); err == nil {
			t.Errorf("Crop(%q) succeeded, want error", in)
		}
	}
}

// ---- Crop: encoding behavior ----

func TestCropQualityDefaultAndExplicit(t *testing.T) {
	orig := newTestJPEG(t)
	cDefault, err := Crop(orig, []Region{baseRegion}, 0) // quality <= 0 falls back to 85
	if err != nil {
		t.Fatalf("Crop default quality: %v", err)
	}
	c85, err := Crop(orig, []Region{baseRegion}, 85)
	if err != nil {
		t.Fatalf("Crop quality 85: %v", err)
	}
	if !bytes.Equal(cDefault.JPEG(), c85.JPEG()) {
		t.Error("quality<=0 should encode identically to quality 85")
	}
}

// TestCropImageMatchesCrop asserts CropImage (crop from an already-decoded
// raster) and Crop (which decodes a JPEG first, then delegates to CropImage)
// produce byte-identical crop JPEGs and SHAs when given the SAME source raster
// and the same regions/quality — i.e. CropImage is a faithful pixel core for
// Crop, not a divergent implementation.
//
// This does NOT mean the F8 RenderFile migration preserves crop bytes from
// before: pre-F8, the ID crop was taken from jpeg.Decode(page.JPEG) (a decode of
// the re-encoded page); post-F8, RenderFile crops directly from the pre-encode
// render raster (RenderPageImage), skipping that extra encode/decode round-trip.
// Those are two different source images, so crop JPEGs/SHAs legitimately
// changed across the migration (higher fidelity, one fewer lossy round-trip).
// Nothing depends on the old crop bytes — id_crop_ref is only ever a blob-key
// suffix, never compared against a historical value.
func TestCropImageMatchesCrop(t *testing.T) {
	orig := newTestJPEG(t)
	src := decodeJPEG(t, orig)
	regions := []Region{baseRegion, {X: 0.1, Y: 0.7, W: 0.8, H: 0.2, Color: "#4a4a4a"}}
	for _, q := range []int{0, 85, 90} {
		fromBytes, err := Crop(orig, regions, q)
		if err != nil {
			t.Fatalf("Crop q=%d: %v", q, err)
		}
		fromRaster, err := CropImage(src, regions, q)
		if err != nil {
			t.Fatalf("CropImage q=%d: %v", q, err)
		}
		if !bytes.Equal(fromBytes.JPEG(), fromRaster.JPEG()) {
			t.Errorf("q=%d: CropImage JPEG bytes differ from Crop", q)
		}
		if fromBytes.SHA256() != fromRaster.SHA256() {
			t.Errorf("q=%d: CropImage SHA %s != Crop SHA %s", q, fromRaster.SHA256(), fromBytes.SHA256())
		}
	}
}

// TestCropImageEmptyRegionsErrors mirrors Crop's contract for the raster entry
// point: no regions is a caller bug (a crop of nothing), not a silent empty crop.
func TestCropImageEmptyRegionsErrors(t *testing.T) {
	src := decodeJPEG(t, newTestJPEG(t))
	if _, err := CropImage(src, nil, 0); err == nil {
		t.Error("CropImage with nil regions succeeded, want error")
	}
}

func TestCropSHA256MatchesJPEGBytes(t *testing.T) {
	orig := newTestJPEG(t)
	crop, err := Crop(orig, []Region{baseRegion}, 0)
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	if crop.SHA256() == "" {
		t.Error("SHA256() is empty")
	}
	if len(crop.SHA256()) != 64 {
		t.Errorf("SHA256() length = %d, want 64 (hex sha256)", len(crop.SHA256()))
	}
}

// ---- LoadIDCrop ----

func TestLoadIDCropRejectsNonIDCropKey(t *testing.T) {
	b := newTestJPEG(t)
	for _, key := range []string{
		"assessments/7/pages/3/original.jpg",
		"idcrop", // no "/idcrop/" path segment
		"",
		"assessments/7/masked/3.jpg", // masked, not idcrop
	} {
		if _, err := LoadIDCrop(key, b); err == nil {
			t.Errorf("LoadIDCrop(%q) succeeded, want error (idcrop-only invariant)", key)
		}
	}
}

// TestLoadIDCropRejectsMaskedKey documents that a key containing BOTH
// segments is pathological and rejected by LoadIDCrop: identity crops and
// masked answer images are never the same artifact (D19 XOR invariant).
func TestLoadIDCropRejectsMaskedKey(t *testing.T) {
	b := newTestJPEG(t)
	if _, err := LoadIDCrop("scans/7/idcrop/masked/3.jpg", b); err == nil {
		t.Error("LoadIDCrop accepted a key containing \"/masked/\", want error")
	}
}

func TestLoadIDCropRoundTrip(t *testing.T) {
	b := newTestJPEG(t)
	c, err := LoadIDCrop("scans/7/idcrop/3.jpg", b)
	if err != nil {
		t.Fatalf("LoadIDCrop: %v", err)
	}
	if !bytes.Equal(c.JPEG(), b) {
		t.Error("JPEG() does not round-trip the input bytes")
	}
}

func TestLoadIDCropRejectsEmptyBytes(t *testing.T) {
	if _, err := LoadIDCrop("a/idcrop/1.jpg", nil); err == nil {
		t.Error("LoadIDCrop with empty bytes succeeded, want error")
	}
}

// TestLoadMaskedRejectsIDCropOnlyKey pins existing LoadMasked behavior: a key
// with an "/idcrop/" segment but no "/masked/" segment is still rejected by
// LoadMasked (LoadMasked's gate was already exact; this documents that
// generalizing to IDCrop did not loosen it).
func TestLoadMaskedRejectsIDCropOnlyKey(t *testing.T) {
	b := newTestJPEG(t)
	if _, err := LoadMasked("scans/7/idcrop/3.jpg", b); err == nil {
		t.Error("LoadMasked accepted an \"/idcrop/\"-only key, want error")
	}
}

// ---- ProviderImage: the sealed interface (D19) ----

// Compile-time guards: MaskedImage and IDCrop both satisfy ProviderImage.
// Because sealedProviderImage is unexported, no type outside this package
// can implement ProviderImage — attempting to do so from another package is
// a compile error, which cannot itself be expressed as a passing test; these
// assignments are the closest positive proof available.
var (
	_ ProviderImage = MaskedImage{}
	_ ProviderImage = IDCrop{}
)

func TestIDCropIsZero(t *testing.T) {
	var zero IDCrop
	if !zero.IsZero() {
		t.Error("zero-value IDCrop.IsZero() = false, want true")
	}
	if zero.JPEG() != nil {
		t.Error("zero-value JPEG() should be nil")
	}
	if zero.SHA256() != "" {
		t.Error("zero-value SHA256() should be empty")
	}
}
