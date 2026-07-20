package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	xdraw "golang.org/x/image/draw"
)

// DownscaleLongEdge decodes a JPEG, scales it (aspect-preserving) so its
// longer edge is at most maxLongEdge pixels, and re-encodes at the given
// JPEG quality. An image already at or under the cap is left at its original
// dimensions but is still re-encoded at quality — callers should not assume
// byte-identity with the input in that case.
//
// This is the "compressed" quality option's downscale step (report spec §3,
// D44: long edge 1600px, JPEG q75) — kept in internal/imaging alongside Mask
// and Crop's own JPEG-in/JPEG-out shape rather than duplicated in
// internal/report, since it operates on the same already-rendered page JPEGs
// those functions consume.
//
// quality <= 0 falls back to defaultQuality (85), matching Mask/Crop.
func DownscaleLongEdge(originalJPEG []byte, maxLongEdge, quality int) ([]byte, error) {
	if maxLongEdge <= 0 {
		return nil, fmt.Errorf("imaging: DownscaleLongEdge requires maxLongEdge > 0, got %d", maxLongEdge)
	}
	src, err := jpeg.Decode(bytes.NewReader(originalJPEG))
	if err != nil {
		return nil, fmt.Errorf("imaging: decode original JPEG: %w", err)
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("imaging: degenerate image %dx%d", w, h)
	}

	long := w
	if h > long {
		long = h
	}
	out := src
	if long > maxLongEdge {
		scale := float64(maxLongEdge) / float64(long)
		newW := int(float64(w)*scale + 0.5)
		newH := int(float64(h)*scale + 0.5)
		if newW < 1 {
			newW = 1
		}
		if newH < 1 {
			newH = 1
		}
		dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
		out = dst
	}

	if quality <= 0 {
		quality = defaultQuality
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("imaging: encode downscaled JPEG: %w", err)
	}
	return buf.Bytes(), nil
}
