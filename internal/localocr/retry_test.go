package localocr

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/HaoWen46/adagrade/internal/ocr"
)

// The retry decision rule (docs: low-confidence retry): ReadLines runs the
// crop once; only when the best line confidence is below retryConfidence (or
// nothing was read at all) does it re-run inference on up to two alternative
// preprocessings of the same crop — border shave and contrast stretch — and
// keep the highest-confidence result. Confident reads stay one pass.
//
// These tests fake the inference seam (recognizeFn) with a scripted
// recognizer, so they cover the orchestration without ONNX Runtime. The
// fixtures are built so splitLines finds exactly ONE band per pass, making
// recognizer-call count == pass count.

// scriptedRec returns scripted (text, conf, err) triples in call order.
type scriptedRec struct {
	calls   int
	replies []struct {
		text string
		conf float64
		err  error
	}
}

func (s *scriptedRec) add(text string, conf float64, err error) *scriptedRec {
	s.replies = append(s.replies, struct {
		text string
		conf float64
		err  error
	}{text, conf, err})
	return s
}

func (s *scriptedRec) rec(_ image.Image) (string, float64, error) {
	i := s.calls
	s.calls++
	if i >= len(s.replies) {
		last := s.replies[len(s.replies)-1]
		return last.text, last.conf, last.err
	}
	r := s.replies[i]
	return r.text, r.conf, r.err
}

// oneBandGray builds a w x h white image with a single dark bar that
// splitLines resolves to exactly one band — and still one band after the
// production shave/stretch variants (the bar sits well inside the shave
// margin).
func oneBandGray(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	y0, y1 := h/2-6, h/2+6
	for y := y0; y < y1; y++ {
		for x := w / 5; x < w*4/5; x++ {
			img.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	return img
}

func TestReadLinesRetry_ConfidentReadIsSinglePass(t *testing.T) {
	s := (&scriptedRec{}).add("B11902003", retryConfidence, nil) // exactly at threshold: confident
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 {
		t.Fatalf("confident read must stay one pass; recognizer ran %d times", s.calls)
	}
	if len(lines) != 1 || lines[0].Text != "B11902003" || lines[0].Confidence != retryConfidence {
		t.Fatalf("confident result altered: %+v", lines)
	}
}

func TestReadLinesRetry_LowConfidenceKeepsBestVariant(t *testing.T) {
	// Base is low-confidence; variant 1 (shave) is better but still below the
	// threshold; variant 2 (stretch) is best. Highest confidence wins.
	s := (&scriptedRec{}).
		add("1B11902003", 0.89, nil). // base: box-edge artifact shape
		add("B11902OO3", 0.91, nil).  // shave variant
		add("B11902003", 0.93, nil)   // stretch variant: best
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 3 {
		t.Fatalf("want base + 2 variant passes = 3 recognizer calls, got %d", s.calls)
	}
	if len(lines) != 1 || lines[0].Text != "B11902003" || lines[0].Confidence != 0.93 {
		t.Fatalf("want the highest-confidence variant result, got %+v", lines)
	}
}

func TestReadLinesRetry_KeepsBaseWhenVariantsAreWorse(t *testing.T) {
	s := (&scriptedRec{}).
		add("B11902003", 0.90, nil).
		add("B1190200", 0.85, nil).
		add("811902003", 0.60, nil)
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 3 {
		t.Fatalf("want 3 passes, got %d", s.calls)
	}
	if len(lines) != 1 || lines[0].Text != "B11902003" || lines[0].Confidence != 0.90 {
		t.Fatalf("worse variants must not replace the base read, got %+v", lines)
	}
}

func TestReadLinesRetry_EmptyReadTriggersRetry(t *testing.T) {
	// No line read at all (empty text is skipped -> zero lines, confidence 0)
	// must trigger the retry even though there is no "confidence" to compare.
	s := (&scriptedRec{}).
		add("", 0, nil).
		add("B11902003", 0.55, nil). // below threshold but better than nothing
		add("", 0, nil)
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 3 {
		t.Fatalf("want 3 passes, got %d", s.calls)
	}
	if len(lines) != 1 || lines[0].Text != "B11902003" {
		t.Fatalf("variant rescue of an empty read lost: %+v", lines)
	}
}

func TestReadLinesRetry_AllEmptyStaysEmpty(t *testing.T) {
	// A genuinely blank crop (all passes empty) must return zero lines, same
	// as the pre-retry behavior — a blank box is a CORRECT empty read.
	s := (&scriptedRec{}).add("", 0, nil)
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("blank crop must stay empty, got %+v", lines)
	}
	if s.calls != 3 {
		t.Fatalf("empty read retries both variants: want 3 passes, got %d", s.calls)
	}
}

func TestReadLinesRetry_EarlyExitOnConfidentRescue(t *testing.T) {
	// If the first variant already rescues the read to a confident level,
	// the second variant pass is skipped (cost bound: "up to two").
	s := (&scriptedRec{}).
		add("1B11902003", 0.89, nil).
		add("B11902003", 0.99, nil) // first variant: confident rescue
	lines, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 2 {
		t.Fatalf("confident rescue must skip the remaining variant: want 2 passes, got %d", s.calls)
	}
	if len(lines) != 1 || lines[0].Text != "B11902003" {
		t.Fatalf("got %+v", lines)
	}
}

func TestReadLinesRetry_VariantErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	s := (&scriptedRec{}).
		add("x", 0.10, nil).
		add("", 0, boom)
	_, err := readLinesRetry(context.Background(), oneBandGray(100, 60), s.rec)
	if !errors.Is(err, boom) {
		t.Fatalf("variant-pass error must propagate (engine close / ORT failure), got %v", err)
	}
}

func TestReadLinesRetry_ContextCancelStopsVariants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scriptedRec{}
	s.add("x", 0.10, nil)
	// Cancel after the base pass by wrapping the recognizer.
	rec := func(img image.Image) (string, float64, error) {
		out, conf, err := s.rec(img)
		cancel()
		return out, conf, err
	}
	_, err := readLinesRetry(ctx, oneBandGray(100, 60), rec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled from the variant pass, got %v", err)
	}
	if s.calls != 1 {
		t.Fatalf("no recognizer call may happen after cancellation, got %d", s.calls)
	}
}

func TestReadLinesRetry_NilVariantConsumesNoPass(t *testing.T) {
	// A crop too small to shave (shaveBorder returns nil) skips that variant
	// entirely; only the stretch pass runs.
	img := image.NewGray(image.Rect(0, 0, 20, 10))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 4; y < 6; y++ {
		for x := 4; x < 16; x++ {
			img.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	if shaveBorder(img) != nil {
		t.Fatal("precondition: 20x10 crop must be too small to shave")
	}
	if contrastStretch(img) == nil {
		t.Fatal("precondition: fixture must survive contrastStretch")
	}
	s := (&scriptedRec{}).add("x", 0.10, nil)
	if _, err := readLinesRetry(context.Background(), img, s.rec); err != nil {
		t.Fatal(err)
	}
	if s.calls != 2 {
		t.Fatalf("nil variant must be skipped: want base + stretch = 2 passes, got %d", s.calls)
	}
}

func TestBestLineConfidence(t *testing.T) {
	if got := bestLineConfidence(nil); got != 0 {
		t.Fatalf("empty: got %v", got)
	}
	lines := []ocr.Line{{Text: "a", Confidence: 0.3}, {Text: "b", Confidence: 0.8}, {Text: "c", Confidence: 0.5}}
	if got := bestLineConfidence(lines); got != 0.8 {
		t.Fatalf("want max 0.8, got %v", got)
	}
}

// --- variant transforms ----------------------------------------------------

func TestShaveBorder_RemovesProportionalMargin(t *testing.T) {
	// 100x60: shave = 12% of min(w,h)=60 -> 7px per side -> 86x46.
	img := image.NewGray(image.Rect(0, 0, 100, 60))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	img.SetGray(7, 7, color.Gray{Y: 42})   // lands at (0,0) after the shave
	img.SetGray(0, 0, color.Gray{Y: 0})    // border pixel: must be gone
	img.SetGray(99, 59, color.Gray{Y: 0})  // border pixel: must be gone
	img.SetGray(50, 30, color.Gray{Y: 10}) // center: survives at (43,23)

	out := shaveBorder(img)
	if out == nil {
		t.Fatal("want a shaved image, got nil")
	}
	if got := out.Bounds(); got.Dx() != 86 || got.Dy() != 46 || got.Min != (image.Point{}) {
		t.Fatalf("want 86x46 at origin, got %v", got)
	}
	if out.GrayAt(0, 0).Y != 42 {
		t.Fatalf("pixel mapping off: (7,7) should land at (0,0), got %d", out.GrayAt(0, 0).Y)
	}
	if out.GrayAt(43, 23).Y != 10 {
		t.Fatalf("center pixel lost: got %d", out.GrayAt(43, 23).Y)
	}
	// Source must not be mutated.
	if img.GrayAt(0, 0).Y != 0 {
		t.Fatal("shaveBorder mutated its input")
	}
}

func TestShaveBorder_TooSmallReturnsNil(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 20, 10))
	if out := shaveBorder(img); out != nil {
		t.Fatalf("a crop too small to shave must return nil, got %v", out.Bounds())
	}
}

func TestShaveBorder_HandlesSubImageBounds(t *testing.T) {
	// A SubImage with a nonzero Min (exactly what jpeg.Decode + SubImage
	// produce in ReadLines) must shave relative to its own bounds. This is
	// the stride/offset trap: raw Pix copies scramble such images.
	base := image.NewGray(image.Rect(0, 0, 200, 120))
	for i := range base.Pix {
		base.Pix[i] = 255
	}
	sub := base.SubImage(image.Rect(50, 30, 150, 90)).(*image.Gray) // 100x60
	base.SetGray(57, 37, color.Gray{Y: 42})                         // sub-local (7,7)
	out := shaveBorder(sub)
	if out == nil {
		t.Fatal("want a shaved image, got nil")
	}
	if got := out.Bounds(); got.Dx() != 86 || got.Dy() != 46 {
		t.Fatalf("want 86x46, got %v", got)
	}
	if out.GrayAt(0, 0).Y != 42 {
		t.Fatalf("SubImage offset handling broken: got %d at origin", out.GrayAt(0, 0).Y)
	}
}

func TestContrastStretch_MapsPercentilesToFullRange(t *testing.T) {
	// 30% ink at 120, 70% background at 220: p5 -> 120, p95 -> 220, so the
	// stretch maps ink to 0 and background to 255.
	img := image.NewGray(image.Rect(0, 0, 100, 10))
	for i := range img.Pix {
		img.Pix[i] = 220
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 30; x++ {
			img.SetGray(x, y, color.Gray{Y: 120})
		}
	}
	out := contrastStretch(img)
	if out == nil {
		t.Fatal("want a stretched image, got nil")
	}
	if got := out.GrayAt(0, 0).Y; got != 0 {
		t.Fatalf("ink (p5 level) must map to 0, got %d", got)
	}
	if got := out.GrayAt(99, 9).Y; got != 255 {
		t.Fatalf("background (p95 level) must map to 255, got %d", got)
	}
	if img.GrayAt(0, 0).Y != 120 {
		t.Fatal("contrastStretch mutated its input")
	}
}

func TestContrastStretch_FlatImageReturnsNil(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 40, 40))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	if out := contrastStretch(img); out != nil {
		t.Fatal("flat image has no contrast to stretch; want nil")
	}
}

func TestContrastStretch_NoInkMassReturnsNil(t *testing.T) {
	// If the low percentile lands in light-background territory (>= 192)
	// there is no real ink to amplify — stretching would only blow JPEG
	// background noise up into full-range garbage. Must decline.
	img := image.NewGray(image.Rect(0, 0, 100, 10))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			img.SetGray(x, y, color.Gray{Y: 240}) // light scanner noise, no ink
		}
	}
	if out := contrastStretch(img); out != nil {
		t.Fatal("noise-only image must not be stretched; want nil")
	}
}

func TestContrastStretch_HandlesSubImageBounds(t *testing.T) {
	base := image.NewGray(image.Rect(0, 0, 200, 40))
	for i := range base.Pix {
		base.Pix[i] = 220
	}
	for y := 10; y < 20; y++ {
		for x := 50; x < 80; x++ {
			base.SetGray(x, y, color.Gray{Y: 120})
		}
	}
	sub := base.SubImage(image.Rect(50, 10, 150, 20)).(*image.Gray) // 100x10, 30% ink
	out := contrastStretch(sub)
	if out == nil {
		t.Fatal("want a stretched image, got nil")
	}
	if got := out.GrayAt(0, 0).Y; got != 0 {
		t.Fatalf("SubImage ink must map to 0, got %d", got)
	}
	if got := out.GrayAt(99, 9).Y; got != 255 {
		t.Fatalf("SubImage background must map to 255, got %d", got)
	}
}
