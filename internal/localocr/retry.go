package localocr

import (
	"context"
	"image"
	"image/color"

	"github.com/HaoWen46/adagrade/internal/ocr"
)

// Low-confidence retry: recognition quality on real ID-box crops.
//
// The TA-drawn crop region deliberately includes a margin around the printed
// header box (padding on the id_region), so the crop usually carries the
// printed box BORDER. On the live messy pile that border reads as a spurious
// leading character — "1B11902003" for B11902003, "【B11902001", "CB99999999"
// — at best-line confidence 0.87–0.91, while clean or rescued reads land at
// 0.965–0.997 (live evidence, demo-scan-pile-messy.pdf through the real
// intake geometry; see TestLive_RetryRescuesBoxEdgeArtifacts). When the one
// cheap pass is confident nothing else runs; when it is not (or nothing was
// read at all), ReadLines re-runs inference on up to two alternative
// preprocessings of the same crop and keeps the highest-confidence result.
const (
	// retryConfidence is the best-line confidence below which the retry
	// fires. Chosen from the live evidence gap: every observed box-edge
	// artifact read scored <= 0.9138 and every clean/rescued read >= 0.965,
	// so 0.94 sits between them with margin on both sides. (The synthetic
	// basicfont fixtures read at 0.98+ regardless and never retry.)
	retryConfidence = 0.94

	// shaveFrac is the per-side border shave, as a fraction of the crop's
	// smaller dimension. The printed box border sits in the outer ~4–13% of
	// the live crops (region padding 0.01 of the page vs a 0.06–0.08 header
	// band); shaving 12% removes it entirely: on the live pile a ~11%
	// (20px/177px) shave rescued every artifact page to the exact ID at
	// confidence 0.995+, while 7% (12px) left the artifact and 9% (16px)
	// left border residue that read as a stray CJK glyph.
	shaveFrac = 0.12
	// shaveMin is the smallest useful shave in pixels.
	shaveMin = 3

	// stretchLoPct/stretchHiPct are the percentile anchors of the contrast
	// stretch. p5→0, p95→255 fixed faint synthetic reads (0.9876→0.9959)
	// and cleaned several live box-border reads outright.
	stretchLoPct = 0.05
	stretchHiPct = 0.95
	// stretchMaxLo declines the stretch when the low percentile lands in
	// light-background territory: there is no real ink mass to amplify, and
	// stretching would only blow scanner/JPEG background noise up into
	// full-range garbage. Faint pencil (~150–190) stays eligible.
	stretchMaxLo = 192
)

// recognizeFn is the inference seam between the crop-level retry orchestration
// and the model: Engine.recognize in production, a scripted fake in unit tests.
type recognizeFn func(img image.Image) (string, float64, error)

// retryVariants are the alternative preprocessings, in the order tried. The
// shave goes first because on the live pile it is the decisive fix (exact ID
// at 0.995+), letting the confident-rescue early exit skip the second pass.
// A variant may return nil (not applicable to this crop); that consumes no
// inference pass. Otsu binarization and 2x upscaling were evaluated and
// rejected: both produced higher-confidence WRONG reads on live crops (Otsu
// misread a digit of a neighbor page's ID; upscaling promoted the "1" border
// artifact above an otherwise-strippable "【" read).
var retryVariants = [...]func(*image.Gray) *image.Gray{shaveBorder, contrastStretch}

// readLinesRetry runs the banded recognition once and, only when the result is
// weak (best line confidence below retryConfidence, or no line read at all),
// re-runs it on the retry variants of the same crop, keeping the highest-
// confidence result. Cost bound: at most two extra passes, zero on confident
// reads; a variant pass that reaches retryConfidence stops the loop.
func readLinesRetry(ctx context.Context, gray *image.Gray, rec recognizeFn) ([]ocr.Line, error) {
	best, err := readBands(ctx, gray, rec)
	if err != nil {
		return nil, err
	}
	bestConf := bestLineConfidence(best)
	if bestConf >= retryConfidence {
		return best, nil
	}
	for _, variant := range retryVariants {
		v := variant(gray)
		if v == nil {
			continue // not applicable to this crop; no pass consumed
		}
		lines, err := readBands(ctx, v, rec)
		if err != nil {
			return nil, err
		}
		if c := bestLineConfidence(lines); c > bestConf {
			best, bestConf = lines, c
			if bestConf >= retryConfidence {
				break // confident rescue; skip any remaining variant
			}
		}
	}
	return best, nil
}

// readBands splits gray into line bands and recognizes each one: exactly one
// inference pass over the crop. Empty recognitions are skipped; lines come
// back in top-to-bottom order.
func readBands(ctx context.Context, gray *image.Gray, rec recognizeFn) ([]ocr.Line, error) {
	bands := splitLines(gray)
	lines := make([]ocr.Line, 0, len(bands))
	for _, r := range bands {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, conf, err := rec(gray.SubImage(r))
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		lines = append(lines, ocr.Line{Text: text, Confidence: conf})
	}
	return lines, nil
}

// bestLineConfidence is the maximum line confidence, 0 when nothing was read.
func bestLineConfidence(lines []ocr.Line) float64 {
	best := 0.0
	for _, l := range lines {
		if l.Confidence > best {
			best = l.Confidence
		}
	}
	return best
}

// shaveBorder returns a copy of g with shaveFrac of its smaller dimension
// (at least shaveMin px) cut from every side, dropping box-border lines that
// ride the crop margins. Returns nil when the crop is too small to shave
// meaningfully. The copy is written pixel-by-pixel via GrayAt/SetGray so
// SubImage inputs (nonzero Min, padded stride) are handled correctly.
func shaveBorder(g *image.Gray) *image.Gray {
	b := g.Bounds()
	m := b.Dx()
	if b.Dy() < m {
		m = b.Dy()
	}
	n := int(shaveFrac * float64(m))
	if n < shaveMin {
		n = shaveMin
	}
	if b.Dx() <= 2*n+4 || b.Dy() <= 2*n+4 {
		return nil
	}
	r := image.Rect(b.Min.X+n, b.Min.Y+n, b.Max.X-n, b.Max.Y-n)
	out := image.NewGray(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			out.SetGray(x, y, g.GrayAt(r.Min.X+x, r.Min.Y+y))
		}
	}
	return out
}

// contrastStretch maps the [p5, p95] luminance range of g onto [0, 255],
// clamping outside it: faint ink goes to full black, light background (and
// thin, JPEG-softened box-border lines) toward full white. Returns nil when
// there is nothing to stretch — a flat image, or a low percentile in light-
// background territory (>= stretchMaxLo, i.e. no real ink mass).
func contrastStretch(g *image.Gray) *image.Gray {
	b := g.Bounds()
	n := b.Dx() * b.Dy()
	if n == 0 {
		return nil
	}
	var hist [256]int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			hist[g.GrayAt(x, y).Y]++
		}
	}
	lo, hi := grayPercentiles(&hist, n, stretchLoPct, stretchHiPct)
	if hi <= lo || lo >= stretchMaxLo {
		return nil
	}
	scale := 255.0 / float64(hi-lo)
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			v := float64(g.GrayAt(b.Min.X+x, b.Min.Y+y).Y)
			s := (v - float64(lo)) * scale
			if s < 0 {
				s = 0
			}
			if s > 255 {
				s = 255
			}
			out.SetGray(x, y, color.Gray{Y: uint8(s + 0.5)})
		}
	}
	return out
}

// grayPercentiles returns the luminance levels at the plo and phi cumulative
// fractions of a 256-bin histogram over n pixels.
func grayPercentiles(hist *[256]int, n int, plo, phi float64) (lo, hi uint8) {
	loN := int(plo * float64(n))
	hiN := int(phi * float64(n))
	acc := 0
	seenLo := false
	for v := 0; v < 256; v++ {
		acc += hist[v]
		if !seenLo && acc >= loN {
			lo = uint8(v)
			seenLo = true
		}
		if acc >= hiN {
			hi = uint8(v)
			break
		}
	}
	return lo, hi
}
