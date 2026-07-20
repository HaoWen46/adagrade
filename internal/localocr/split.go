package localocr

import "image"

// Splitter tuning. These are pixel/heuristic constants for the ink-density
// projection; they are intentionally lenient because the input is a small,
// clean, TA-drawn box, not a full page.
const (
	// minBandHeight drops bands thinner than this many rows (specks, rule
	// lines) rather than treating them as text.
	minBandHeight = 4
	// mergeGap merges two ink bands separated by at most this many blank rows
	// (descenders, anti-aliasing) into one line.
	mergeGap = 3
	// vpad is the vertical padding added above/below each band before it is
	// cropped, so ascenders/descenders are not clipped.
	vpad = 2
	// hpad is the horizontal padding kept around the trimmed ink columns.
	hpad = 2
)

// splitLines recovers 1–3 line bands from a grayscale ID-box crop by a
// horizontal ink-density projection (docs/DECISIONS.md D24, recognition-only:
// no detection model). It adaptively thresholds against the image mean, counts
// ink pixels per row, groups contiguous above-noise rows into bands, merges
// bands separated by tiny gaps, pads each band vertically, and trims each
// band's left/right whitespace by a per-band column projection.
//
// If no band clears the thresholds (e.g. a blank crop), it falls back to the
// whole crop as a single band so the recognizer still gets a chance.
func splitLines(img *image.Gray) []image.Rectangle {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return []image.Rectangle{b}
	}

	thr := meanThreshold(img)

	// Per-row ink counts (pixels darker than the threshold).
	rowInk := make([]int, h)
	for y := 0; y < h; y++ {
		n := 0
		for x := 0; x < w; x++ {
			if img.GrayAt(b.Min.X+x, b.Min.Y+y).Y < thr {
				n++
			}
		}
		rowInk[y] = n
	}

	// Noise floor: a row counts as "inky" if it has more than this. A small
	// fraction of the width tolerates stray specks / faint rule lines.
	floor := w / 40 // 2.5% of width

	bands := contiguousBands(rowInk, floor)
	bands = mergeClose(bands, mergeGap)
	bands = dropShort(bands, minBandHeight)

	if len(bands) == 0 {
		return []image.Rectangle{b} // whole-crop fallback
	}

	rects := make([]image.Rectangle, 0, len(bands))
	for _, bd := range bands {
		y0 := clamp(bd.lo-vpad, 0, h)
		y1 := clamp(bd.hi+vpad, 0, h)
		x0, x1 := trimColumns(img, y0, y1, thr, floorCols(h))
		// Translate back to the image's coordinate origin.
		r := image.Rect(b.Min.X+x0, b.Min.Y+y0, b.Min.X+x1, b.Min.Y+y1)
		if r.Dx() > 0 && r.Dy() > 0 {
			rects = append(rects, r)
		}
	}
	if len(rects) == 0 {
		return []image.Rectangle{b}
	}
	return rects
}

// band is a half-open row range [lo, hi) in image-local coordinates.
type band struct{ lo, hi int }

// meanThreshold returns an adaptive threshold: the mean luminance. Pixels below
// it are treated as ink. For a light box with dark text this cleanly separates
// foreground from background without a magic constant.
func meanThreshold(img *image.Gray) uint8 {
	b := img.Bounds()
	var sum uint64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum += uint64(img.GrayAt(x, y).Y)
		}
	}
	n := uint64(b.Dx() * b.Dy())
	if n == 0 {
		return 128
	}
	return uint8(sum / n)
}

// contiguousBands groups runs of rows whose ink count exceeds floor.
func contiguousBands(rowInk []int, floor int) []band {
	var out []band
	inBand := false
	start := 0
	for y, n := range rowInk {
		if n > floor {
			if !inBand {
				inBand = true
				start = y
			}
		} else if inBand {
			out = append(out, band{lo: start, hi: y})
			inBand = false
		}
	}
	if inBand {
		out = append(out, band{lo: start, hi: len(rowInk)})
	}
	return out
}

// mergeClose merges adjacent bands separated by at most gap blank rows.
func mergeClose(bands []band, gap int) []band {
	if len(bands) == 0 {
		return bands
	}
	out := []band{bands[0]}
	for _, bd := range bands[1:] {
		last := &out[len(out)-1]
		if bd.lo-last.hi <= gap {
			last.hi = bd.hi
		} else {
			out = append(out, bd)
		}
	}
	return out
}

// dropShort removes bands thinner than minH rows.
func dropShort(bands []band, minH int) []band {
	out := bands[:0]
	for _, bd := range bands {
		if bd.hi-bd.lo >= minH {
			out = append(out, bd)
		}
	}
	return out
}

// trimColumns finds the left/right ink extent within rows [y0,y1) and returns a
// padded [x0,x1) column range. Falls back to the full width if the band has no
// column clearing floorCols.
func trimColumns(img *image.Gray, y0, y1 int, thr uint8, floorC int) (int, int) {
	b := img.Bounds()
	w := b.Dx()
	left, right := -1, -1
	for x := 0; x < w; x++ {
		n := 0
		for y := y0; y < y1; y++ {
			if img.GrayAt(b.Min.X+x, b.Min.Y+y).Y < thr {
				n++
			}
		}
		if n > floorC {
			if left < 0 {
				left = x
			}
			right = x + 1
		}
	}
	if left < 0 {
		return 0, w // no ink columns; keep full width
	}
	return clamp(left-hpad, 0, w), clamp(right+hpad, 0, w)
}

// floorCols is the per-column ink floor for horizontal trimming: a small
// fraction of the band-independent crop height keeps single thin strokes.
func floorCols(h int) int { return h / 40 }

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
