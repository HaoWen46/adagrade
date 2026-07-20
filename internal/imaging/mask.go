// Package imaging produces masked (redacted) page images for the grading
// pipeline. It is the sole constructor of MaskedImage, the only type the
// vision-provider layer accepts (docs/DECISIONS.md D10): sending an unmasked
// image to a provider is a compile error, not a convention.
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

// defaultQuality is the JPEG encode quality used when the caller passes
// quality <= 0 (spec §7: "quality ~85").
const defaultQuality = 85

// defaultFill is the fallback mask color (#4a4a4a). A dark gray is preferred
// over pure black because some vision models over-attend to pure-black boxes
// (spec §7).
var defaultFill = color.RGBA{R: 0x4a, G: 0x4a, B: 0x4a, A: 0xff}

// Region is one normalized (0..1 fractions of width/height) redaction
// rectangle. Regions are DPI-independent: the same Region masks the same
// content at any render resolution.
type Region struct {
	X, Y, W, H float64
	Color      string  // hex like "#4a4a4a"; invalid/empty falls back to #4a4a4a
	Padding    float64 // normalized padding added on all four sides
}

// ProviderImage is the sealed interface the vision-provider layer accepts
// (docs/DECISIONS.md D10, generalized by D19). A provider request carries
// EITHER answer content with identity masked out (MaskedImage) XOR identity
// with no answer content (IDCrop) — never both, and never an arbitrary
// unmasked page. sealedProviderImage is unexported, so only types declared in
// this package can implement ProviderImage: sending an unmasked image to a
// provider is a compile error, not a convention.
type ProviderImage interface {
	JPEG() []byte
	SHA256() string
	sealedProviderImage()
}

// MaskedImage is a masked derivative of a page image. It is the ONLY type the
// vision-provider layer accepts (docs/DECISIONS.md D10): both fields are
// unexported so a MaskedImage can only be produced by this package — via Mask
// (fresh masking) or LoadMasked (the audited storage read-back gate).
type MaskedImage struct {
	jpeg []byte
	sha  string
}

// JPEG returns the masked JPEG bytes.
func (m MaskedImage) JPEG() []byte { return m.jpeg }

// SHA256 returns the lowercase hex SHA-256 of the masked JPEG bytes.
func (m MaskedImage) SHA256() string { return m.sha }

// IsZero reports whether m is the zero value (no masked bytes).
func (m MaskedImage) IsZero() bool { return len(m.jpeg) == 0 && m.sha == "" }

// sealedProviderImage marks MaskedImage as a ProviderImage (D19). See the
// ProviderImage doc comment: this method's unexported name is the seal.
func (m MaskedImage) sealedProviderImage() {}

// Mask decodes originalJPEG, draws each region as a solid rectangle (color,
// padding, clamped to bounds via Intersect), and re-encodes as JPEG at the
// given quality (<= 0 means 85). The original bytes are never mutated.
//
// Mask is a pure function: the same input bytes + regions + quality yield
// byte-identical output (Go's image/jpeg encoder is deterministic for a given
// Go version), so the masked artifact is idempotent and its SHA-256 is stable.
//
// A region whose padded pixel rect does not intersect the image is skipped,
// not an error. An empty regions slice produces a re-encoded copy — still a
// valid MaskedImage.
func Mask(originalJPEG []byte, regions []Region, quality int) (MaskedImage, error) {
	src, err := jpeg.Decode(bytes.NewReader(originalJPEG))
	if err != nil {
		return MaskedImage{}, fmt.Errorf("imaging: decode original JPEG: %w", err)
	}

	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	wPx := float64(bounds.Dx())
	hPx := float64(bounds.Dy())
	for _, r := range regions {
		x0 := int(math.Floor((r.X - r.Padding) * wPx))
		y0 := int(math.Floor((r.Y - r.Padding) * hPx))
		x1 := int(math.Ceil((r.X + r.W + r.Padding) * wPx))
		y1 := int(math.Ceil((r.Y + r.H + r.Padding) * hPx))
		rect := image.Rect(x0, y0, x1, y1).Add(bounds.Min).Intersect(bounds)
		if rect.Empty() {
			continue // fully outside the image: nothing to cover
		}
		draw.Draw(dst, rect, image.NewUniform(parseHexColor(r.Color)), image.Point{}, draw.Src)
	}

	if quality <= 0 {
		quality = defaultQuality
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return MaskedImage{}, fmt.Errorf("imaging: encode masked JPEG: %w", err)
	}
	out := buf.Bytes()
	sum := sha256.Sum256(out)
	return MaskedImage{jpeg: out, sha: hex.EncodeToString(sum[:])}, nil
}

// LoadMasked wraps ALREADY-MASKED bytes read back from storage (the grading
// worker path re-reads the stored masked artifact instead of re-masking).
//
// The key must contain the masked-artifact naming convention ("/masked/"
// path segment). This check is the single audited gate for reconstructing a
// MaskedImage outside Mask: per docs/DECISIONS.md D10, MaskedImage's fields
// are unexported precisely so that unmasked bytes cannot reach the provider
// layer, and this function is the only other constructor. Gating it on the
// masked-artifact key convention means every MaskedImage in the system is
// either freshly masked here (Mask) or was previously written under a
// "/masked/" storage key — an original-image key can never be wrapped.
func LoadMasked(key string, jpegBytes []byte) (MaskedImage, error) {
	if !strings.Contains(key, "/masked/") {
		return MaskedImage{}, fmt.Errorf(
			"imaging: key %q is not a masked artifact (missing \"/masked/\" segment); refusing to wrap (D10 masked-only invariant)", key)
	}
	if len(jpegBytes) == 0 {
		return MaskedImage{}, errors.New("imaging: empty masked bytes")
	}
	// Copy so a caller mutating its slice cannot desync bytes and SHA.
	cp := make([]byte, len(jpegBytes))
	copy(cp, jpegBytes)
	sum := sha256.Sum256(cp)
	return MaskedImage{jpeg: cp, sha: hex.EncodeToString(sum[:])}, nil
}

// parseHexColor parses "#rgb" or "#rrggbb". Anything else — including an
// empty string — falls back to defaultFill rather than erroring: a bad color
// preference must never block redaction.
func parseHexColor(s string) color.RGBA {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '#' {
		return defaultFill
	}
	hexDigits := s[1:]
	var r, g, b uint8
	var ok bool
	switch len(hexDigits) {
	case 3: // #rgb → #rrggbb
		var rn, gn, bn uint8
		if rn, ok = hexNibble(hexDigits[0]); !ok {
			return defaultFill
		}
		if gn, ok = hexNibble(hexDigits[1]); !ok {
			return defaultFill
		}
		if bn, ok = hexNibble(hexDigits[2]); !ok {
			return defaultFill
		}
		r, g, b = rn*0x11, gn*0x11, bn*0x11
	case 6: // #rrggbb
		if r, ok = hexByte(hexDigits[0], hexDigits[1]); !ok {
			return defaultFill
		}
		if g, ok = hexByte(hexDigits[2], hexDigits[3]); !ok {
			return defaultFill
		}
		if b, ok = hexByte(hexDigits[4], hexDigits[5]); !ok {
			return defaultFill
		}
	default:
		return defaultFill
	}
	return color.RGBA{R: r, G: g, B: b, A: 0xff}
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func hexByte(hi, lo byte) (uint8, bool) {
	h, ok1 := hexNibble(hi)
	l, ok2 := hexNibble(lo)
	return h<<4 | l, ok1 && ok2
}
