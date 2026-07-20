package localocr

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/ocr"
	"github.com/HaoWen46/adagrade/internal/render"
)

// TestLive_ReadLines is an opt-in end-to-end test against the real ONNX
// recognizer. It is skipped unless all three asset env vars are set:
//
//	ADAMARKER_OCR_MODEL   -> ch_PP-OCRv4_rec_infer.onnx
//	ADAMARKER_OCR_KEYS    -> ppocr_keys_v1.txt
//	ADAMARKER_ONNXRUNTIME -> libonnxruntime.{so,dylib}
//
// It renders the student-ID string "B11902156" in black on white using the
// ASCII-only basicfont, scales it ~4x so the 7x13 glyphs are not microscopic,
// wraps the JPEG through imaging.LoadIDCrop (the audited crop gate), runs
// ReadLines, and asserts the concatenated recognized text contains the ID after
// case-folding. basicfont is ASCII-only, so only the Latin+digit half of the
// model is exercised here; the Chinese-name half is validated in real use (a
// unit test cannot synthesize CJK glyphs without shipping a CJK font).
func TestLive_ReadLines(t *testing.T) {
	model := os.Getenv("ADAMARKER_OCR_MODEL")
	keys := os.Getenv("ADAMARKER_OCR_KEYS")
	lib := os.Getenv("ADAMARKER_ONNXRUNTIME")
	if model == "" || keys == "" || lib == "" {
		t.Skip("live OCR test skipped: set ADAMARKER_OCR_MODEL, ADAMARKER_OCR_KEYS, ADAMARKER_ONNXRUNTIME to run")
	}

	const want = "B11902156"
	jpegBytes := renderIDJPEG(t, want, 4)

	crop, err := imaging.LoadIDCrop("test/idcrop/live.jpg", jpegBytes)
	if err != nil {
		t.Fatalf("LoadIDCrop: %v", err)
	}

	eng, err := New(Config{ModelPath: model, KeysPath: keys, ONNXRuntimeLibPath: lib})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	lines, err := eng.ReadLines(context.Background(), crop)
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}

	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Text)
	}
	// Fold case and strip spaces so "b1 1902156" style spacing/casing noise
	// does not fail an otherwise-correct read.
	got := strings.ReplaceAll(strings.ToUpper(sb.String()), " ", "")
	// NOTE: recognized text is PII in production (D14) and is never logged
	// there; it is asserted here only because this is a synthetic fixture, not
	// a real student ID.
	if !strings.Contains(got, want) {
		t.Fatalf("recognized %q does not contain %q (%d line(s))", got, want, len(lines))
	}
}

// TestLive_RetryRescuesBoxEdgeArtifacts is the box-edge-artifact live test.
//
// It reproduces the intake pipeline on the committed demo pile
// data/demo/demo-scan-pile-messy.pdf: render each page at the intake defaults
// (render.Options zero value, 250 DPI), crop the student-ID header box with
// the exact region the Guide walkthrough seeds (scripts/seed-demo-walkthrough.py:
// x=0.05 y=0.02 w=0.25 h=0.06 padding=0.01 — the padded crop deliberately
// includes the printed box border), and run ReadLines.
//
// WITHOUT the low-confidence retry, these crops reproduce the live-observed
// failure shapes: the box border reads as a spurious leading character
// ("1B11902003", "【B11902001", "CB99999999") at confidence ~0.87-0.91. The
// test asserts the retry path IMPROVES those reads to the exact printed ID and
// PRESERVES the correct empty read on the blank/empty-ID pages. Before/after
// text+confidence are logged for the report.
//
// Page indices and expected IDs are fixed because the pile is committed
// byte-identical (make-demo-data.py renders it with a fixed SEED2 shuffle and
// invariant=1). All IDs/names here are synthetic demo fixtures, not student
// PII (same D14 justification as TestLive_ReadLines).
func TestLive_RetryRescuesBoxEdgeArtifacts(t *testing.T) {
	model := os.Getenv("ADAMARKER_OCR_MODEL")
	keys := os.Getenv("ADAMARKER_OCR_KEYS")
	lib := os.Getenv("ADAMARKER_ONNXRUNTIME")
	if model == "" || keys == "" || lib == "" {
		t.Skip("live OCR test skipped: set ADAMARKER_OCR_MODEL, ADAMARKER_OCR_KEYS, ADAMARKER_ONNXRUNTIME to run")
	}
	pdf, err := os.ReadFile("../../data/demo/demo-scan-pile-messy.pdf")
	if err != nil {
		t.Fatalf("read demo pile: %v", err)
	}

	eng, err := New(Config{ModelPath: model, KeysPath: keys, ONNXRuntimeLibPath: lib})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	rdr, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("pdfium init: %v", err)
	}
	defer rdr.Close()
	ctx := context.Background()
	doc, err := rdr.Open(ctx, pdf)
	if err != nil {
		t.Fatalf("open pile: %v", err)
	}
	defer doc.Close()

	// The student_id region the walkthrough seeds (see doc comment).
	region := imaging.Region{X: 0.05, Y: 0.02, W: 0.25, H: 0.06, Padding: 0.01}

	// Page -> printed ID in the committed pile (empty string = the blank page
	// and the empty-ID-box page, which must KEEP their correct empty reads).
	wantByPage := map[int]string{
		1:  "B11902001",
		2:  "B99999999", // off-roster page: still must read cleanly
		3:  "B11902004",
		4:  "",
		5:  "B11902003", // the live-observed "1B11902003" artifact page
		6:  "B11902005",
		7:  "B11902002",
		8:  "",
		9:  "B11902001",
		10: "B11902002",
		11: "B11902006",
	}

	fold := func(lines []ocr.Line) string {
		var sb strings.Builder
		for _, l := range lines {
			sb.WriteString(l.Text)
		}
		return strings.ReplaceAll(strings.ToUpper(sb.String()), " ", "")
	}

	for page := 0; page < 12; page++ {
		want, checked := wantByPage[page]
		raster, _, err := doc.RenderPageImage(ctx, page, render.Options{})
		if err != nil {
			t.Fatalf("page %d: render: %v", page, err)
		}
		crop, err := imaging.CropImage(raster, []imaging.Region{region}, 0)
		if err != nil {
			t.Fatalf("page %d: crop: %v", page, err)
		}

		// Before: one plain pass (no retry), through the same decode gate.
		src, err := jpeg.Decode(bytes.NewReader(crop.JPEG()))
		if err != nil {
			t.Fatalf("page %d: decode crop: %v", page, err)
		}
		before, err := readBands(ctx, toGray(src), eng.recognize)
		if err != nil {
			t.Fatalf("page %d: base pass: %v", page, err)
		}

		// After: the production path (retry included).
		after, err := eng.ReadLines(ctx, crop)
		if err != nil {
			t.Fatalf("page %d: ReadLines: %v", page, err)
		}

		beforeText, beforeConf := fold(before), bestLineConfidence(before)
		afterText, afterConf := fold(after), bestLineConfidence(after)
		t.Logf("page %2d before: %-14q conf=%.4f | after: %-14q conf=%.4f",
			page, beforeText, beforeConf, afterText, afterConf)

		if !checked {
			continue // scribbled-ID page: garbage in, garbage out; nothing to assert
		}
		if want == "" {
			if len(after) != 0 {
				t.Errorf("page %d: blank/empty-ID crop must stay an empty read, got %q", page, afterText)
			}
			continue
		}
		if afterText != want {
			t.Errorf("page %d: retried read = %q, want exactly %q (before: %q)", page, afterText, want, beforeText)
		}
		if afterConf < beforeConf {
			t.Errorf("page %d: retry regressed confidence: before %.4f after %.4f", page, beforeConf, afterConf)
		}
	}
}

// renderIDJPEG draws s in black basicfont on a white background, upscaled by
// integer factor scale (nearest-neighbor so glyph edges stay crisp), and
// returns JPEG bytes.
func renderIDJPEG(t *testing.T, s string, scale int) []byte {
	t.Helper()
	face := basicfont.Face7x13

	// Base canvas: measure the string width, add a margin.
	adv := font.MeasureString(face, s).Ceil()
	pad := 8
	bw := adv + 2*pad
	bh := face.Metrics().Height.Ceil() + 2*pad

	base := image.NewGray(image.Rect(0, 0, bw, bh))
	draw.Draw(base, base.Bounds(), image.NewUniform(color.Gray{Y: 255}), image.Point{}, draw.Src)

	d := &font.Drawer{
		Dst:  base,
		Src:  image.NewUniform(color.Gray{Y: 0}),
		Face: face,
		Dot:  fixed.P(pad, pad+face.Metrics().Ascent.Ceil()),
	}
	d.DrawString(s)

	// Integer upscale with nearest-neighbor (no smoothing) so thin strokes
	// survive at recognizer resolution.
	up := image.NewGray(image.Rect(0, 0, bw*scale, bh*scale))
	for y := 0; y < up.Bounds().Dy(); y++ {
		for x := 0; x < up.Bounds().Dx(); x++ {
			up.SetGray(x, y, base.GrayAt(x/scale, y/scale))
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, up, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode fixture JPEG: %v", err)
	}
	return buf.Bytes()
}
