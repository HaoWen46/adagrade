package imaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"strings"
)

// IDCrop is a tight crop of the student-identification box(es) on a page,
// containing identity (name + student ID) and NO answer content — the other
// half of the D19 XOR invariant (MaskedImage carries answer content with
// identity masked out; IDCrop carries identity with answer content excluded
// entirely). Both fields are unexported so an IDCrop can only be produced by
// this package — via Crop (fresh cropping) or LoadIDCrop (the audited
// storage read-back gate).
type IDCrop struct {
	jpeg []byte
	sha  string
}

// JPEG returns the cropped JPEG bytes.
func (c IDCrop) JPEG() []byte { return c.jpeg }

// SHA256 returns the lowercase hex SHA-256 of the cropped JPEG bytes.
func (c IDCrop) SHA256() string { return c.sha }

// IsZero reports whether c is the zero value (no cropped bytes).
func (c IDCrop) IsZero() bool { return len(c.jpeg) == 0 && c.sha == "" }

// sealedProviderImage marks IDCrop as a ProviderImage (D19). See
// ProviderImage's doc comment in mask.go: this method's unexported name is
// the seal.
func (c IDCrop) sealedProviderImage() {}

// Crop decodes originalJPEG, extracts each region's padded pixel rect (same
// floor/ceil ± padding math as Mask, clamped to bounds via Intersect), and
// stacks the extracted rects vertically into a single image: width is the
// max of the individual crop widths, height is their sum, and any crop
// narrower than the stack is left-aligned against a white background. The
// stack is re-encoded as JPEG at the given quality (<= 0 means 85).
//
// Unlike Mask, a region whose padded pixel rect does not intersect the image
// is an ERROR, not skipped: Mask silently covering nothing is harmless
// (there was nothing there to leak), but Crop silently producing an empty
// slice would hand the identification step a crop of nothing, which is a
// caller bug (a misconfigured region) that must surface immediately rather
// than fail an OCR call downstream with an inscrutable blank image. An empty
// regions slice is likewise an error — there is nothing to crop.
func Crop(originalJPEG []byte, regions []Region, quality int) (IDCrop, error) {
	if len(regions) == 0 {
		return IDCrop{}, errors.New("imaging: Crop requires at least one region")
	}
	src, err := jpeg.Decode(bytes.NewReader(originalJPEG))
	if err != nil {
		return IDCrop{}, fmt.Errorf("imaging: decode original JPEG: %w", err)
	}
	return CropImage(src, regions, quality)
}

// CropImage is Crop's pixel core, taking an already-decoded raster instead of
// JPEG bytes. It is the F8 fix: scan renders a page once and crops the ID box
// from that raster, so the second jpeg.Decode of the JPEG it just encoded
// disappears. Crop delegates here after decoding, so for the SAME source pixels
// both entry points produce byte-identical crop JPEGs (same floor/ceil ± padding
// math, same vertical stacking, same encode) — CropImage is not a divergent
// implementation of Crop. This says nothing about matching crop bytes from
// before the F8 migration: RenderFile now feeds CropImage the pre-encode render
// raster instead of a decode of the re-encoded page JPEG, a different source
// image, so post-migration crop bytes/SHAs intentionally differ from the old
// path (higher fidelity, one fewer lossy round-trip) — nothing depends on the
// old bytes. It shares Crop's contract: an empty regions slice, or any region
// whose padded rect falls fully outside the image, is an error (a crop of
// nothing is a caller bug). IDCrop stays sealed — this is still inside the
// imaging package, so D19's "constructible only here" holds.
func CropImage(src image.Image, regions []Region, quality int) (IDCrop, error) {
	if len(regions) == 0 {
		return IDCrop{}, errors.New("imaging: CropImage requires at least one region")
	}
	bounds := src.Bounds()

	wPx := float64(bounds.Dx())
	hPx := float64(bounds.Dy())
	rects := make([]image.Rectangle, len(regions))
	maxW := 0
	totalH := 0
	for i, r := range regions {
		x0 := int(math.Floor((r.X - r.Padding) * wPx))
		y0 := int(math.Floor((r.Y - r.Padding) * hPx))
		x1 := int(math.Ceil((r.X + r.W + r.Padding) * wPx))
		y1 := int(math.Ceil((r.Y + r.H + r.Padding) * hPx))
		rect := image.Rect(x0, y0, x1, y1).Add(bounds.Min).Intersect(bounds)
		if rect.Empty() {
			return IDCrop{}, fmt.Errorf("imaging: region %d is fully outside the image bounds (a crop of nothing is a bug)", i)
		}
		rects[i] = rect
		if w := rect.Dx(); w > maxW {
			maxW = w
		}
		totalH += rect.Dy()
	}

	dst := image.NewRGBA(image.Rect(0, 0, maxW, totalH))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	y := 0
	for _, rect := range rects {
		target := image.Rect(0, y, rect.Dx(), y+rect.Dy())
		draw.Draw(dst, target, src, rect.Min, draw.Src)
		y += rect.Dy()
	}

	if quality <= 0 {
		quality = defaultQuality
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return IDCrop{}, fmt.Errorf("imaging: encode cropped JPEG: %w", err)
	}
	out := buf.Bytes()
	sum := sha256.Sum256(out)
	return IDCrop{jpeg: out, sha: hex.EncodeToString(sum[:])}, nil
}

// LoadIDCrop wraps ALREADY-CROPPED bytes read back from storage, mirroring
// LoadMasked's role for MaskedImage.
//
// The key must contain the crop-artifact naming convention ("/idcrop/" path
// segment) and must NOT also contain "/masked/" — a key satisfying both
// segments is pathological (masked answer images and identity crops are
// never the same artifact per the D19 XOR invariant), so LoadIDCrop rejects
// it rather than silently picking a side. This function and Crop are the
// only constructors of IDCrop, mirroring LoadMasked/Mask's gate on
// MaskedImage (docs/DECISIONS.md D10, D19).
func LoadIDCrop(key string, jpegBytes []byte) (IDCrop, error) {
	if !strings.Contains(key, "/idcrop/") {
		return IDCrop{}, fmt.Errorf(
			"imaging: key %q is not an idcrop artifact (missing \"/idcrop/\" segment); refusing to wrap (D19 identity-crop invariant)", key)
	}
	if strings.Contains(key, "/masked/") {
		return IDCrop{}, fmt.Errorf(
			"imaging: key %q contains both \"/idcrop/\" and \"/masked/\" segments; refusing to wrap (D19 identity XOR answer-content invariant)", key)
	}
	if len(jpegBytes) == 0 {
		return IDCrop{}, errors.New("imaging: empty idcrop bytes")
	}
	// Copy so a caller mutating its slice cannot desync bytes and SHA.
	cp := make([]byte, len(jpegBytes))
	copy(cp, jpegBytes)
	sum := sha256.Sum256(cp)
	return IDCrop{jpeg: cp, sha: hex.EncodeToString(sum[:])}, nil
}
