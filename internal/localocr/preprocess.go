package localocr

import (
	"image"
	"image/color"
	"image/draw"

	xdraw "golang.org/x/image/draw"
)

// channelOrder selects how the three input planes are filled. PP-OCR / Paddle
// lineage models are usually trained on BGR (OpenCV's default read order), so
// BGR is the default here on the strength of that upstream convention.
//
// EMPIRICAL NOTE (D24 live E2E, ONNX Runtime 1.27.0, ch_PP-OCRv4_rec): the
// synthetic fixture is grayscale (R==G==B), so it decodes "B11902156" at conf
// 0.98 under BOTH orders and therefore does NOT discriminate between them — the
// BGR choice rests on the Paddle convention, not on that test. If a future
// live check on a genuinely colored crop decodes garbage, switch the default in
// preprocess() to orderRGB and record which won here.
type channelOrder int

const (
	orderBGR channelOrder = iota
	orderRGB
)

// preprocess converts a grayscale line-band image into a CHW float32 tensor for
// the recognizer (docs/DECISIONS.md D24).
//
// Steps: resize to height h preserving aspect ratio; cap the resulting width at
// maxW; if the width is below minW, pad on the right with WHITE (a light
// background, matching the paper) up to minW. Pixels are normalized by
// (x/255 - 0.5)/0.5, mapping white(255)->+1 and black(0)->-1. The output is
// laid out NCHW: [1, 3, h, W] with the three planes identical for a grayscale
// source (channel order is irrelevant for grey, but the plane count matches the
// model's 3-channel input). The returned shape is []int64{1,3,h,W}.
//
// minW may be 0 to disable the floor. The channel order is fixed at the default
// (BGR) here; the RGB fallback lives in preprocessOrder.
func preprocess(src image.Image, h, maxW, minW int) ([]float32, []int64) {
	return preprocessOrder(src, h, maxW, minW, orderBGR)
}

func preprocessOrder(src image.Image, h, maxW, minW int, order channelOrder) ([]float32, []int64) {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 || h == 0 {
		// Degenerate: emit a 1-wide white tensor so callers never divide by 0.
		return whiteTensor(h, max(minW, 1)), []int64{1, 3, int64(h), int64(max(minW, 1))}
	}

	// Aspect-preserving width at the target height.
	w := int(float64(h)*float64(sw)/float64(sh) + 0.5)
	if w < 1 {
		w = 1
	}
	if maxW > 0 && w > maxW {
		w = maxW
	}

	// Resize into an RGBA canvas over a white background (so any letterbox
	// from aspect rounding is light, not black).
	resized := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(resized, resized.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), src, sb, xdraw.Over, nil)

	outW := w
	if minW > 0 && outW < minW {
		outW = minW
	}

	// NCHW float32: plane0, plane1, plane2 each h*outW, row-major.
	plane := h * outW
	data := make([]float32, 3*plane)
	// Initialize to white(+1) so the right-pad region [w,outW) is light.
	for i := range data {
		data[i] = 1
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := resized.At(x, y).RGBA() // 16-bit
			rf := norm(uint8(r >> 8))
			gf := norm(uint8(g >> 8))
			bf := norm(uint8(b >> 8))
			var c0, c1, c2 float32
			switch order {
			case orderRGB:
				c0, c1, c2 = rf, gf, bf
			default: // orderBGR
				c0, c1, c2 = bf, gf, rf
			}
			idx := y*outW + x
			data[idx] = c0
			data[plane+idx] = c1
			data[2*plane+idx] = c2
		}
	}
	return data, []int64{1, 3, int64(h), int64(outW)}
}

// norm applies the PP-OCR normalization (x/255 - 0.5)/0.5 == x/127.5 - 1.
func norm(v uint8) float32 {
	return float32(float64(v)/127.5 - 1.0)
}

// whiteTensor returns an all-white (+1) NCHW tensor body of size 3*h*w.
func whiteTensor(h, w int) []float32 {
	d := make([]float32, 3*h*w)
	for i := range d {
		d[i] = 1
	}
	return d
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
