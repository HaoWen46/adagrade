package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/jpeg" // decode side effects: register the JPEG format
	_ "image/png"  // decode side effects: register the PNG format
	"net/http"

	"github.com/HaoWen46/adagrade/internal/render"
)

// NormalizeImage decodes a single-page raster submission (PNG or JPEG, stdlib
// decoders only), downscales it so its long edge is at most opts.MaxLongEdgePx
// (aspect ratio preserved, never upscaled), and re-encodes it as JPEG at
// opts.JPEGQuality. The result is the ONE rendered page for an image submission
// (source_kind='image', page_count=1; spec §8, D22).
func NormalizeImage(data []byte, opts render.Options) (render.Page, error) {
	_, page, err := NormalizeImageRaster(data, opts)
	return page, err
}

// NormalizeImageRaster is NormalizeImage that also returns the scaled raster it
// encoded (F8): a caller that must crop the ID box (scan's image path) can do so
// from this raster with imaging.CropImage instead of jpeg-Decoding the JPEG this
// just produced. The returned Page is byte-identical to NormalizeImage's (same
// downscale + encode path — image SHAs are unchanged), and the raster is the
// exact image those bytes were encoded from.
func NormalizeImageRaster(data []byte, opts render.Options) (image.Image, render.Page, error) {
	opts = imageOptsWithDefaults(opts)
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, render.Page{}, fmt.Errorf("ingest: decode image: %w", err)
	}

	scaled := downscaleToLongEdge(img, opts.MaxLongEdgePx)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: opts.JPEGQuality}); err != nil {
		return nil, render.Page{}, fmt.Errorf("ingest: encode jpeg: %w", err)
	}
	b := scaled.Bounds()
	sum := sha256.Sum256(buf.Bytes())
	return scaled, render.Page{
		JPEG:   buf.Bytes(),
		Width:  b.Dx(),
		Height: b.Dy(),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// imageOptsWithDefaults mirrors render.Options.withDefaults (unexported outside the
// render package) for the values NormalizeImage needs; keeps the same defaults
// (render.DefaultMaxLongEdgePx, render.DefaultJPEGQuality) as the PDF render path.
func imageOptsWithDefaults(o render.Options) render.Options {
	if o.MaxLongEdgePx <= 0 {
		o.MaxLongEdgePx = render.DefaultMaxLongEdgePx
	}
	if o.JPEGQuality <= 0 {
		o.JPEGQuality = render.DefaultJPEGQuality
	}
	return o
}

// downscaleToLongEdge box-samples img down so its longer side is at most maxLongEdge
// pixels, preserving aspect ratio. Images already within the cap are returned
// unchanged (this path never upscales). Stdlib-only (no golang.org/x/image
// dependency in go.mod), so this is a simple box-filter resize: each destination
// pixel averages the source pixels in its corresponding source-space rectangle,
// which anti-aliases reasonably well for downscaling scanned pages/photos.
func downscaleToLongEdge(img image.Image, maxLongEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxLongEdge <= 0 {
		return img
	}
	longEdge := w
	if h > longEdge {
		longEdge = h
	}
	if longEdge <= maxLongEdge {
		return img
	}

	scale := float64(maxLongEdge) / float64(longEdge)
	dstW := max(1, int(float64(w)*scale+0.5))
	dstH := max(1, int(float64(h)*scale+0.5))

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	scaleXInv := float64(w) / float64(dstW)
	scaleYInv := float64(h) / float64(dstH)
	for dy := 0; dy < dstH; dy++ {
		sy0 := int(float64(dy) * scaleYInv)
		sy1 := int(float64(dy+1) * scaleYInv)
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		if sy1 > h {
			sy1 = h
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := int(float64(dx) * scaleXInv)
			sx1 := int(float64(dx+1) * scaleXInv)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sx1 > w {
				sx1 = w
			}
			var rSum, gSum, bSum, aSum, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, bl, a := img.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					rSum += uint64(r)
					gSum += uint64(g)
					bSum += uint64(bl)
					aSum += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			// image.Color.RGBA() returns 16-bit-per-channel premultiplied values;
			// downshift by 8 to fit the 8-bit RGBA destination.
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(rSum / n >> 8), G: uint8(gSum / n >> 8),
				B: uint8(bSum / n >> 8), A: uint8(aSum / n >> 8),
			})
		}
	}
	return dst
}

// sniffImageExt returns the file extension ("png" or "jpg") for the sniffed content
// type of raw image bytes, defaulting to "jpg" for anything else.
func sniffImageExt(data []byte) string {
	if http.DetectContentType(data) == "image/png" {
		return "png"
	}
	return "jpg"
}
