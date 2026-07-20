package report

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/image/font/gofont/goregular"

	"github.com/HaoWen46/adagrade/internal/render"
)

// testFontPath writes the (non-CJK, but real UTF-8-capable) Go Regular TTF
// bundled with golang.org/x/image to a temp file and returns its path — a
// stand-in for `make report-fonts`'s Noto Sans TC in tests that only need
// SOME valid TTF to exercise layout, not actual CJK glyph coverage (that is
// exercised separately by the ADAMARKER_REPORT_FONT-gated CJK test using the
// real configured font).
func testFontPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-font.ttf")
	if err := os.WriteFile(path, goregular.TTF, 0o600); err != nil {
		t.Fatalf("write test font: %v", err)
	}
	return path
}

// solidJPEG returns a JPEG filled with a single color at the given
// dimensions — used as a stand-in "student page image".
func solidJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test page JPEG: %v", err)
	}
	return buf.Bytes()
}

// inkyJPEG returns a page-like JPEG: a white background with a dark
// horizontal band drawn across the middle, so both "does this half have any
// content" (a solid color, e.g. the grading panel's white background would
// be indistinguishable from blank) AND "is there visible ink" checks have
// something real to probe. Used for the PDF round-trip non-blank test.
func inkyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	bandTop := h / 3
	bandBottom := 2 * h / 3
	draw.Draw(img, image.Rect(0, bandTop, w, bandBottom), image.NewUniform(color.Black), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode inky JPEG: %v", err)
	}
	return buf.Bytes()
}

func sampleInput() ReportInput {
	return ReportInput{
		AssessmentName: "Midterm 2",
		StudentName:    "Ada Lovelace",
		StudentID:      "B11902999",
		Total:          "27",
		Max:            "30",
		Quality:        QualityOriginal,
		Problems: []ProblemReport{
			{
				Label: "Problem 1: Determinants",
				Pages: [][]byte{}, // filled by caller in most tests
				Criteria: []CriterionLine{
					{Name: "Setup", Score: "5", Max: "5", Comment: "Correct expansion."},
					{Name: "Arithmetic", Score: "8", Max: "10", Comment: "Sign error in row 2."},
				},
				Total: "13",
				Max:   "15",
			},
			{
				Label: "Problem 2: Eigenvalues",
				Pages: [][]byte{},
				Criteria: []CriterionLine{
					{Name: "Characteristic polynomial", Score: "8", Max: "8"},
					{Name: "Eigenvectors", Score: "6", Max: "7", Comment: "Missing normalization."},
				},
				Total: "14",
				Max:   "15",
			},
		},
	}
}

// ---- Build: validation ----

func TestBuild_RejectsInvalidQuality(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{B: 200, A: 255})}
	in.Quality = "fast" // not a real option
	if _, err := Build(testFontPath(t), in); err == nil {
		t.Fatal("Build should reject an invalid Quality value")
	}
}

func TestBuild_RejectsEmptyFontPath(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{B: 200, A: 255})}
	if _, err := Build("", in); err == nil {
		t.Fatal("Build should reject an empty fontPath — callers must gate on config.ReportFontConfigured")
	}
}

func TestBuild_RejectsNoProblems(t *testing.T) {
	in := sampleInput()
	in.Problems = nil
	if _, err := Build(testFontPath(t), in); err == nil {
		t.Fatal("Build should reject a ReportInput with no problems")
	}
}

func TestBuild_RejectsProblemWithNoPages(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = nil // still no pages
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{B: 200, A: 255})}
	if _, err := Build(testFontPath(t), in); err == nil {
		t.Fatal("Build should reject a problem with zero pages")
	}
}

// TestBuild_RejectsCFFBasedOTFImmediately guards the sfnt-magic-bytes check
// in newDoc: a CFF-based OpenType (.otf, "OTTO") font must be rejected right
// away with a clear error, not silently accepted only to fail later with a
// confusing "undefined font" error the first time text is drawn (fpdf's own
// AddUTF8FontFromBytes error path does not call f.SetError on this failure —
// see newDoc's doc comment).
func TestBuild_RejectsCFFBasedOTFImmediately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.otf")
	// "OTTO" + padding: enough for newDoc's 4-byte sfnt-version check to see
	// a non-TrueType tag without needing a real multi-megabyte OTF fixture.
	if err := os.WriteFile(path, []byte("OTTO"+strings.Repeat("\x00", 32)), 0o600); err != nil {
		t.Fatalf("write fake OTF: %v", err)
	}

	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{B: 200, A: 255})}

	_, err := Build(path, in)
	if err == nil {
		t.Fatal("Build should reject a CFF-based OTF font immediately")
	}
	if !strings.Contains(err.Error(), "TrueType") {
		t.Errorf("error should explain the TrueType-vs-OTF distinction, got: %v", err)
	}
}

func TestBuild_RejectsMissingFontFile(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{B: 200, A: 255})}
	if _, err := Build(filepath.Join(t.TempDir(), "does-not-exist.ttf"), in); err == nil {
		t.Fatal("Build should error when the font file does not exist")
	}
}

// ---- Build: produces a well-formed PDF ----

func TestBuild_ProducesNonEmptyPDFBytes(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 400, 600, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 500, 350, color.RGBA{B: 200, A: 255})}
	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Build returned empty bytes")
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output does not start with a PDF header, got first bytes: %q", out[:min(20, len(out))])
	}
}

// TestBuild_PageCountMatchesProblemPages asserts the PDF has exactly one
// page per answer-page image across all problems (spec §3: "image pages run
// sequentially"). It rasterizes the built PDF back via internal/render (the
// project's real pdfium renderer) rather than parsing PDF structure by hand.
func TestBuild_PageCountMatchesProblemPages(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{
		inkyJPEG(t, 400, 600),
		inkyJPEG(t, 400, 600), // multi-page answer -> "(continued)" on page 2
	}
	in.Problems[1].Pages = [][]byte{
		inkyJPEG(t, 500, 350),
	}
	wantPages := 3

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := r.PageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("PageCount on built PDF: %v", err)
	}
	if got != wantPages {
		t.Errorf("page count = %d, want %d (one PDF page per answer-page image)", got, wantPages)
	}
}

// TestBuild_LeftAndRightHalvesAreNonBlank rasterizes the built PDF's first
// page and checks BOTH halves carry visible content: the left half (the
// student's inky page image) and the right half (the grading panel's text).
// This is the layout smoke test the brief calls for — "assert page count +
// that left and right halves are non-blank (ink-presence heuristic)".
func TestBuild_LeftAndRightHalvesAreNonBlank(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{inkyJPEG(t, 400, 600)}
	in.Problems[1].Pages = [][]byte{inkyJPEG(t, 500, 350)}

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	page := rasterizeFirstPage(t, out)
	leftFrac := inkFraction(page, leftHalfRect(page.Bounds()))
	rightFrac := inkFraction(page, rightHalfRect(page.Bounds()))

	if leftFrac < 0.01 {
		t.Errorf("left half ink fraction = %.4f, want > 0.01 (student page image should be visible)", leftFrac)
	}
	if rightFrac < 0.001 {
		t.Errorf("right half ink fraction = %.4f, want > 0.001 (grading panel text should be visible)", rightFrac)
	}
}

// TestBuild_ContinuedPageIsNonBlank asserts a problem's second (continuation)
// page still renders a non-blank left half (the next page image) and some
// right-half content (the "(continued)" marker), matching spec §3's
// multi-page answer behavior.
func TestBuild_ContinuedPageIsNonBlank(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{inkyJPEG(t, 400, 600), inkyJPEG(t, 400, 600)}
	in.Problems[1].Pages = [][]byte{inkyJPEG(t, 400, 600)}

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	page, err := r.RenderPage(context.Background(), out, 1, render.Options{DPI: 100, MaxLongEdgePx: 4000, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("render page 2 (continuation page): %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(page.JPEG))
	if err != nil {
		t.Fatalf("decode rendered page: %v", err)
	}
	leftFrac := inkFraction(img, leftHalfRect(img.Bounds()))
	rightFrac := inkFraction(img, rightHalfRect(img.Bounds()))
	if leftFrac < 0.01 {
		t.Errorf("continuation page left half ink fraction = %.4f, want > 0.01", leftFrac)
	}
	if rightFrac < 0.0001 {
		t.Errorf("continuation page right half ink fraction = %.4f, want > 0.0001 (\"(continued)\" text)", rightFrac)
	}
}

// ---- panel pagination (finding: layout.go drawPanel + SetAutoPageBreak(false)
// silently let a tall grading panel — many criteria or long comments — run
// past the page bottom, where pdfium clips it and the subtotal is lost) ----

// longCriteriaFixture returns n criteria, each with a long (but ASCII —
// CJK-safe wrapping is covered separately by the ADAMARKER_REPORT_FONT-gated
// live test) comment, long enough that a handful of them overflow a single
// grading panel page (empirically ~10-14 such criteria exhaust A4-landscape
// half-page height at this package's font sizes/line height).
func longCriteriaFixture(n int) []CriterionLine {
	const longComment = "This criterion's comment is intentionally long so that it wraps across several lines in the narrow grading panel column, the same way a detailed Chinese-language comment would, in order to exercise vertical overflow of the right-half panel."
	out := make([]CriterionLine, n)
	for i := range out {
		out[i] = CriterionLine{
			Name:    fmt.Sprintf("Criterion %d", i+1),
			Score:   "4",
			Max:     "5",
			Comment: longComment,
		}
	}
	return out
}

// TestBuild_TallPanelSpillsToContinuationPageAndKeepsSubtotal is the red test
// for the panel-pagination finding: a problem with 12 long-comment criteria
// must not have its bottom criteria or subtotal silently clipped off the
// page. It asserts (a) the PDF grew extra page(s) beyond the one page-image
// the problem carries, and (b) the "Subtotal" text lands, ink-visible, on
// SOME page (the continuation page it spills to).
func TestBuild_TallPanelSpillsToContinuationPageAndKeepsSubtotal(t *testing.T) {
	in := sampleInput()
	in.Problems = []ProblemReport{
		{
			Label:    "Problem 1: Many criteria",
			Pages:    [][]byte{inkyJPEG(t, 400, 600)},
			Criteria: longCriteriaFixture(12),
			Total:    "48",
			Max:      "60",
		},
	}

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	pageCount, err := r.PageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("PageCount on built PDF: %v", err)
	}
	if pageCount < 2 {
		t.Fatalf("page count = %d, want >= 2 — a 12-long-comment-criteria panel must spill onto a continuation page instead of being clipped", pageCount)
	}

	// The subtotal must be ink-visible on SOME page: check every page's
	// bottom band (where MultiCell leaves the subtotal after however many
	// criteria preceded it on that page) plus the full right half, so this
	// doesn't depend on exactly which page the pagination logic lands it on.
	foundSubtotalInk := false
	for i := 0; i < pageCount; i++ {
		page := rasterizePage(t, out, i)
		rightRect := rightHalfRect(page.Bounds())
		// A continuation page devoted to the panel is full-width (no image
		// half), so also probe the full page width's bottom band.
		fullRect := page.Bounds()
		bandFrac := inkFraction(page, bottomBandRect(rightRect, 0.5))
		fullBandFrac := inkFraction(page, bottomBandRect(fullRect, 0.5))
		if bandFrac > 0.0005 || fullBandFrac > 0.0005 {
			foundSubtotalInk = true
			break
		}
	}
	if !foundSubtotalInk {
		t.Error("no page's bottom half shows ink where the Subtotal line should land — it was likely clipped off the bottom of a page")
	}
}

// TestBuild_TallPanelContinuationPageIsFullWidth asserts the pagination
// fix's continuation pages are full-width panel-only pages (per the fix
// brief: "full-width panel page (no image half)"), not just a repeat of the
// narrow right-half column. A normal (non-continuation) page's grading
// panel text is confined to the right-half column, so its left half is
// blank; a full-width continuation page's panel text starts at the page's
// own left margin, so — with no image drawn there — the left half now
// carries panel text ink instead of staying blank.
func TestBuild_TallPanelContinuationPageIsFullWidth(t *testing.T) {
	in := sampleInput()
	in.Problems = []ProblemReport{
		{
			Label:    "Problem 1: Many criteria",
			Pages:    [][]byte{inkyJPEG(t, 400, 600)},
			Criteria: longCriteriaFixture(12),
			Total:    "48",
			Max:      "60",
		},
	}

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	pageCount, err := r.PageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("PageCount on built PDF: %v", err)
	}
	if pageCount < 2 {
		t.Fatalf("page count = %d, want >= 2 for this fixture", pageCount)
	}

	// Page index 1 (the second PDF page) is the problem's only panel
	// continuation page candidate here, since the problem has just one
	// page-image (so page 0 carries the image + the panel's first page's
	// worth of content).
	page := rasterizePage(t, out, 1)
	leftFrac := inkFraction(page, leftHalfRect(page.Bounds()))
	if leftFrac < 0.001 {
		t.Errorf("panel continuation page left half ink fraction = %.4f, want > 0.001 (full-width panel text should start at the left margin, not stay confined to the narrow right-half column)", leftFrac)
	}
}

// TestBuild_SmallPanelStaysSinglePage is the regression guard: the existing
// small-panel fixtures (sampleInput's 2 short criteria per problem) must
// still produce exactly one PDF page per answer-page image, matching the
// pre-fix layout — pagination must only kick in when content actually
// overflows.
func TestBuild_SmallPanelStaysSinglePage(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{inkyJPEG(t, 400, 600)}
	in.Problems[1].Pages = [][]byte{inkyJPEG(t, 500, 350)}
	wantPages := 2

	out, err := Build(testFontPath(t), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := r.PageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("PageCount on built PDF: %v", err)
	}
	if got != wantPages {
		t.Errorf("page count = %d, want %d — a small panel must not spuriously paginate", got, wantPages)
	}
}

// ---- helpers: rasterize + ink-presence heuristic (localocr's split.go
// technique, applied here at a coarser page-half granularity) ----

func rasterizeFirstPage(t *testing.T, pdfBytes []byte) image.Image {
	t.Helper()
	return rasterizePage(t, pdfBytes, 0)
}

// rasterizePage rasterizes a single (0-based) page of the built PDF via the
// project's real pdfium renderer — the generalized form of
// rasterizeFirstPage, used by the panel-pagination tests to inspect
// continuation pages by index.
func rasterizePage(t *testing.T, pdfBytes []byte, pageIdx int) image.Image {
	t.Helper()
	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()
	page, err := r.RenderPage(context.Background(), pdfBytes, pageIdx, render.Options{DPI: 100, MaxLongEdgePx: 4000, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("render page %d: %v", pageIdx, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(page.JPEG))
	if err != nil {
		t.Fatalf("decode rendered page: %v", err)
	}
	return img
}

// bottomBandRect returns the bottom slice of rect (fracFromBottom of its
// height) — used to probe specifically for the SUBTOTAL line, which always
// lands as the last thing drawn in the grading panel, rather than the whole
// half (which would also count criteria/comment ink above it).
func bottomBandRect(rect image.Rectangle, fracFromBottom float64) image.Rectangle {
	bandH := int(float64(rect.Dy()) * fracFromBottom)
	if bandH < 1 {
		bandH = 1
	}
	return image.Rect(rect.Min.X, rect.Max.Y-bandH, rect.Max.X, rect.Max.Y)
}

func leftHalfRect(b image.Rectangle) image.Rectangle {
	mid := b.Min.X + b.Dx()/2
	return image.Rect(b.Min.X, b.Min.Y, mid, b.Max.Y)
}

func rightHalfRect(b image.Rectangle) image.Rectangle {
	mid := b.Min.X + b.Dx()/2
	return image.Rect(mid, b.Min.Y, b.Max.X, b.Max.Y)
}

// inkFraction returns the fraction of pixels within rect that are
// "inky" (darker than a mid-gray threshold) — the same adaptive-ink idea
// localocr/split.go uses for line detection, applied here as a coarse
// blank-vs-non-blank signal over a whole page half rather than per-row bands.
func inkFraction(img image.Image, rect image.Rectangle) float64 {
	const threshold = 200 // 0-255 luma; a clean white page is ~255
	total := 0
	inky := 0
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			r, g, bch, _ := img.At(x, y).RGBA()
			// Rec. 601 luma from 16-bit channels.
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(bch>>8)) / 1000
			total++
			if luma < threshold {
				inky++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(inky) / float64(total)
}

// ---- CheckFont (main.go's startup validation hook) ----

func TestCheckFont_AcceptsValidTTF(t *testing.T) {
	if err := CheckFont(testFontPath(t)); err != nil {
		t.Errorf("CheckFont should accept a valid TrueType font: %v", err)
	}
}

func TestCheckFont_RejectsMissingFile(t *testing.T) {
	if err := CheckFont(filepath.Join(t.TempDir(), "nope.ttf")); err == nil {
		t.Error("CheckFont should reject a nonexistent font file")
	}
}

func TestCheckFont_RejectsGarbageBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-font.ttf")
	if err := os.WriteFile(path, []byte("this is not a font file"), 0o600); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
	if err := CheckFont(path); err == nil {
		t.Error("CheckFont should reject bytes that are not a valid TTF")
	}
}

// ---- size guard (spec §3: "any item whose attachment exceeds 15 MB gets a
// per-item warning") ----

func TestExceedsSizeGuard_UnderThreshold(t *testing.T) {
	if ExceedsSizeGuard(make([]byte, 1024)) {
		t.Error("1KB should not exceed the 15MB size guard")
	}
}

func TestExceedsSizeGuard_AtThresholdIsNotExceeding(t *testing.T) {
	// Exactly 15MB is the boundary — "exceeds" means strictly over, so an
	// attachment landing exactly on the cap should not trip the warning.
	if ExceedsSizeGuard(make([]byte, 15*1024*1024)) {
		t.Error("exactly 15MB should not count as exceeding the guard")
	}
}

func TestExceedsSizeGuard_OverThreshold(t *testing.T) {
	if !ExceedsSizeGuard(make([]byte, 15*1024*1024+1)) {
		t.Error("15MB+1 byte should exceed the size guard")
	}
}

// ---- Build: deterministic rebuild-on-resend (spec §3) ----

// sameWidthPagesInput builds a report whose page images all share the EXACT
// same pixel width but carry different content — the real-world collision the
// same-DPI investigation flagged (two different problems' scanned answer pages
// from one batch render to identical widths). This is the input that trips
// fpdf's width-only image sort.
func sameWidthPagesInput(t *testing.T) ReportInput {
	t.Helper()
	const w, h = 1000, 1400
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{
		solidJPEG(t, w, h, color.RGBA{200, 40, 40, 255}),
		solidJPEG(t, w, h, color.RGBA{40, 160, 40, 255}),
	}
	in.Problems[1].Pages = [][]byte{
		solidJPEG(t, w, h, color.RGBA{40, 40, 200, 255}),
	}
	return in
}

// TestBuild_DeterministicWithSameWidthPages pins spec §3's "rebuild-on-resend
// is deterministic" for the case that trips fpdf: 2+ page images decoding to
// the same pixel width. fpdf's putimages() SliceStable-sorts registered images
// by width alone with no tie-break, so equal-width images get object numbers
// assigned in Go map-iteration order (randomized per process) — empirically
// ~1-in-5-to-1-in-10 builds differ. Build must produce byte-identical output
// across many rebuilds regardless.
func TestBuild_DeterministicWithSameWidthPages(t *testing.T) {
	in := sameWidthPagesInput(t)
	font := testFontPath(t)

	first, err := Build(font, in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const rebuilds = 60
	for i := 1; i <= rebuilds; i++ {
		got, err := Build(font, in)
		if err != nil {
			t.Fatalf("Build rebuild %d: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("Build output not byte-identical on rebuild %d/%d with same-width pages (spec §3 resend determinism); len first=%d got=%d", i, rebuilds, len(first), len(got))
		}
	}
}

// TestBuild_PinsModificationDateForResend guards the second, time-based
// non-determinism the tight-loop test above cannot see (its rebuilds land in
// the same wall-clock second): fpdf writes /ModDate from time.Now() unless
// pinned, so a resend minutes after the original send would always differ by
// mod-time even for a single-page report. Build must pin /ModDate to a fixed
// epoch.
func TestBuild_PinsModificationDateForResend(t *testing.T) {
	out, err := Build(testFontPath(t), sameWidthPagesInput(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !bytes.Contains(out, []byte("/ModDate (D:19700101000000)")) {
		found := regexp.MustCompile(`/ModDate \(D:\d+\)`).Find(out)
		t.Fatalf("Build did not pin /ModDate to the epoch — a resend would differ by mod-time; found %q", found)
	}
}
