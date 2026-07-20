package report

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	"os"
	"reflect"
	"strconv"
	"time"
	"unsafe"

	"github.com/go-pdf/fpdf"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// Layout constants (mm, A4 landscape = 297x210). The page is split into a
// left image half and a right grading-panel half, per spec §3.
const (
	pageW = 297.0
	pageH = 210.0

	margin     = 10.0
	gutter     = 6.0 // gap between the two halves
	halfW      = (pageW - 2*margin - gutter) / 2
	contentTop = margin

	// fontFamily is the single registered UTF-8 family name (Noto Sans TC) —
	// both "" (regular) and "B" (bold) styles are loaded from the same TTF
	// file (fpdf synthesizes bold-ish weight from one face when only one is
	// registered per style key; Noto Sans TC ships one static weight here,
	// which is the D42 minimum: "CJK comments require real font embedding",
	// not a full family).
	fontFamily = "NotoSansTC"

	headerFontSize = 14.0
	subFontSize    = 10.0
	panelFontSize  = 10.0
	criterionSize  = 9.5
	continuedSize  = 10.0
	lineHeight     = 5.5
)

// sfntVersionTrueType is the 4-byte version tag fpdf's ttfparser.go accepts
// (\x00\x01\x00\x00 — the classic TrueType sfnt version). "OTTO" marks a
// CFF/PostScript-outline OpenType font, which fpdf explicitly rejects
// ("fonts based on PostScript outlines are not supported").
var sfntVersionTrueType = []byte{0x00, 0x01, 0x00, 0x00}

// newDoc constructs an A4-landscape Fpdf with the report's UTF-8 font
// registered under both "" and "B" styles (fpdf requires an explicit
// AddUTF8Font call per style key it will be asked to SetFont with; the brief
// only mandates Regular, so Bold reuses the same face — text set to "B"
// still renders, just not visually bolder).
//
// The font bytes are read here and handed to AddUTF8FontFromBytes rather
// than AddUTF8Font(family, style, fontPath): fpdf's path-based loader joins
// fontPath onto its internal fontpath (default ".") via path.Join, which
// silently strips a leading "/" off an absolute path — exactly what
// ADAMARKER_REPORT_FONT typically is (spec §3 example:
// "./data/fonts/NotoSansTC-Regular.ttf", but operators may configure an
// absolute path in production). Reading the file directly sidesteps that
// footgun entirely.
//
// The sfnt-version check below exists because fpdf's own error handling for
// this exact failure mode is unreliable: addFontFromBytes's parseFile error
// path (ttfparser.go, "fonts based on PostScript outlines are not
// supported") prints to stdout and returns WITHOUT calling f.SetError —
// verified empirically against a real CFF-based Noto Sans CJK TC .otf, see
// .superpowers/sdd/n3-A-report.md. Left unchecked, pdf.Error() stays nil
// here and the failure only surfaces later as a confusing "undefined font"
// error the first time SetFont/a draw call needs the font. Checking the
// magic bytes ourselves turns that into an immediate, actionable error.
func newDoc(fontPath string) (*fpdf.Fpdf, error) {
	fontBytes, err := os.ReadFile(fontPath)
	if err != nil {
		return nil, fmt.Errorf("report: read font %q: %w", fontPath, err)
	}
	if len(fontBytes) < 4 || !bytes.Equal(fontBytes[:4], sfntVersionTrueType) {
		return nil, fmt.Errorf("report: font %q is not a TrueType (.ttf) font — fpdf requires real glyf-outline TrueType, not CFF-based OpenType (.otf); see `make report-fonts`", fontPath)
	}

	pdf := fpdf.NewCustom(&fpdf.InitType{
		OrientationStr: "L",
		UnitStr:        "mm",
		SizeStr:        "A4",
		Size:           fpdf.SizeType{Wd: pageW, Ht: pageH},
	})
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddUTF8FontFromBytes(fontFamily, "", fontBytes)
	pdf.AddUTF8FontFromBytes(fontFamily, "B", fontBytes)
	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("report: load font %q: %w", fontPath, err)
	}
	pdf.SetFont(fontFamily, "", panelFontSize)
	// fpdf's own output has several sources of non-determinism; all are fixed so
	// Build's bytes depend only on ReportInput + font content — spec §3's
	// deterministic-rebuild-on-resend guarantee (the publish send job rebuilds the
	// same student's attachment from scratch on every resend; the report attachment
	// tests assert byte-identical output for identical inputs):
	//
	//  1. /CreationDate AND /ModDate both default to time.Now() (fpdf.go's putinfo
	//     writes each from a separate field). Pinning only CreationDate left /ModDate
	//     as wall-clock — invisible to a back-to-back rebuild test (same second) but
	//     making a real resend minutes later differ every time. Both are pinned to a
	//     fixed epoch.
	//  2. Object dictionaries (images, fonts, templates) are emitted by ranging over
	//     Go maps in fpdf's internal bookkeeping, whose iteration order is
	//     randomized per-process. SetCatalogSort(true) is fpdf's documented opt-in to
	//     sort those dictionaries before writing — BUT its image sort is by width
	//     alone with no tie-break, so equal-width images (same-DPI scanned pages, the
	//     common case) still get randomized object numbering. That residual is fixed
	//     per-image in drawImageHalf (see nudgeImageSortKey); it cannot be fixed here.
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetCatalogSort(true)
	return pdf, nil
}

// drawReport lays out every problem's pages onto pdf in order (spec §3:
// "problems merged in order"). The very first page of the whole document
// carries the header band; every problem's first page shows its full
// grading panel, later pages of a multi-page answer show "(continued)".
func drawReport(pdf *fpdf.Fpdf, in ReportInput) error {
	if len(in.Problems) == 0 {
		return fmt.Errorf("report: ReportInput has no problems")
	}

	first := true
	imgSeq := 0 // monotonic counter giving every registered fpdf image a unique name
	for _, p := range in.Problems {
		if len(p.Pages) == 0 {
			return fmt.Errorf("report: problem %q has no pages", p.Label)
		}
		for pageIdx, pageJPEG := range p.Pages {
			resolved, err := resolvePageJPEG(pageJPEG, in.Quality)
			if err != nil {
				return err
			}

			pdf.AddPage()
			top := contentTop
			if first {
				top = drawHeader(pdf, in)
				first = false
			}

			imgSeq++
			if err := drawImageHalf(pdf, resolved, top, imgSeq); err != nil {
				return fmt.Errorf("report: problem %q page %d: %w", p.Label, pageIdx+1, err)
			}
			if pageIdx == 0 {
				drawPanel(pdf, p, top)
			} else {
				drawContinuedPanel(pdf, p, top)
			}
		}
	}
	if err := pdf.Error(); err != nil {
		return fmt.Errorf("report: layout: %w", err)
	}
	return nil
}

// drawHeader renders the first-page-only header band (assessment name,
// student name+ID, assessment total — spec §3) and returns the y-coordinate
// content should start below it.
func drawHeader(pdf *fpdf.Fpdf, in ReportInput) float64 {
	y := contentTop
	pdf.SetXY(margin, y)
	pdf.SetFont(fontFamily, "B", headerFontSize)
	pdf.CellFormat(pageW-2*margin, 8, in.AssessmentName, "", 2, "L", false, 0, "")

	pdf.SetFont(fontFamily, "", subFontSize)
	pdf.SetX(margin)
	studentLine := fmt.Sprintf("%s  (%s)", in.StudentName, in.StudentID)
	pdf.CellFormat(pageW-2*margin, 6, studentLine, "", 2, "L", false, 0, "")

	if in.Total != "" && in.Max != "" {
		pdf.SetFont(fontFamily, "B", subFontSize)
		pdf.SetX(margin)
		pdf.CellFormat(pageW-2*margin, 6, fmt.Sprintf("Total: %s / %s", in.Total, in.Max), "", 2, "L", false, 0, "")
	}

	bottom := pdf.GetY() + 2
	pdf.SetDrawColor(180, 180, 180)
	pdf.SetLineWidth(0.3)
	pdf.Line(margin, bottom, pageW-margin, bottom)
	return bottom + 4
}

// drawImageHalf places the student's page image in the left half, scaled to
// fit within the available width/height while preserving aspect ratio
// (never upscaled past the image's native size scaled to fit — fpdf's
// ImageOptions with only width driven and height computed handles the
// common case, but pages can be taller than wide, so both dimensions are
// checked here and the tighter constraint wins).
func drawImageHalf(pdf *fpdf.Fpdf, pageJPEG []byte, top float64, seq int) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pageJPEG))
	if err != nil {
		return fmt.Errorf("decode page image dimensions: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("degenerate page image %dx%d", cfg.Width, cfg.Height)
	}

	availW := halfW
	availH := pageH - top - margin
	scale := availW / float64(cfg.Width)
	if h := float64(cfg.Height) * scale; h > availH {
		scale = availH / float64(cfg.Height)
	}
	w := float64(cfg.Width) * scale
	h := float64(cfg.Height) * scale

	// Center the image within the left half's box so a narrow/short page
	// doesn't hug the top-left corner awkwardly.
	x := margin + (availW-w)/2
	y := top + (availH-h)/2

	name := fmt.Sprintf("page-%d", seq) // unique per placed image within this document
	info := pdf.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "JPG"}, bytes.NewReader(pageJPEG))
	// Break fpdf putimages()'s width-only image sort (see newDoc note 2) by giving
	// this image a sub-integer-unique sort key. seq is monotonic per placed image,
	// so seq/imageSortKeyDenom is a distinct fraction < 1 for every image in the
	// document — the sort now has a total order, but int(info.w) (the emitted PDF
	// /Width) and the raw JPEG bytes are unchanged, so nothing about the rendered
	// image moves. This is the ONLY content-preserving fix: fpdf exposes no setter
	// and the defect is unfixed upstream (present through v0.12.0), so the
	// alternatives all re-encode the image (breaking the "original" quality option).
	nudgeImageSortKey(info, float64(seq)/imageSortKeyDenom)
	pdf.ImageOptions(name, x, y, w, h, false, fpdf.ImageOptions{ImageType: "JPG"}, 0, "")
	if err := pdf.Error(); err != nil {
		return fmt.Errorf("place page image: %w", err)
	}
	return nil
}

// imageSortKeyDenom scales the per-image sort-key nudge. It must exceed the
// number of images a report can hold so seq/imageSortKeyDenom stays < 1 (the
// fraction must not roll int(info.w) over to a different integer width). 2^16
// is far above any real report's page count; a document somehow exceeding it
// degrades gracefully to fpdf's default (content-equivalent, not byte-identical)
// ordering rather than corrupting a width.
const imageSortKeyDenom = 1 << 16

// nudgeImageSortKey adds `nudge` to fpdf's internal, unexported per-image width
// field (ImageInfoType.w) — the sole key putimages() sorts registered images by
// when assigning PDF object numbers. fpdf provides no setter for it, so this
// reaches the field via reflect+unsafe.
//
// Why not a cleaner fix: the width-sort-with-no-tie-break is an upstream fpdf
// defect (verified present in both v0.9.0 and the latest v0.12.0), unreachable
// through the public API; the only alternatives — a forked/vendored fpdf, or
// padding/re-encoding each page image to a unique width — respectively add a
// whole-library maintenance burden or alter image content (defeating the
// "original" quality option, spec §3/D44). Nudging the float sort key by a
// sub-integer amount changes neither the emitted /Width (int(info.w) truncates
// the fraction away) nor a single byte of the embedded JPEG.
//
// Failure mode is safe: if fpdf ever renames or retypes the field, the reflect
// lookup no-ops and output falls back to the pre-existing content-equivalent-
// but-not-byte-identical ordering — harmless (nothing compares attachment bytes)
// and caught in CI by TestBuild_DeterministicWithSameWidthPages.
func nudgeImageSortKey(info *fpdf.ImageInfoType, nudge float64) {
	if info == nil {
		return
	}
	v := reflect.ValueOf(info).Elem().FieldByName("w")
	if !v.IsValid() || v.Kind() != reflect.Float64 || !v.CanAddr() {
		return
	}
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().SetFloat(v.Float() + nudge)
}

// panelBottom is the lowest y-coordinate panel content may occupy before a
// block must spill onto a continuation page — the usable-height ceiling that
// SetAutoPageBreak(false) (layout.go's newDoc) no longer enforces for us
// (finding: layout.go:212-238 + :89 — with many criteria or long comments the
// panel silently ran past this line and pdfium clipped it).
const panelBottom = pageH - margin

// measuredLines sets pdf's font to (style, size) and returns how many lines
// text wraps to at width w, using fpdf's own UTF-8-aware SplitText — the same
// line-breaking logic MultiCell uses internally, so the count this returns
// matches the height MultiCell will actually consume (spec fix: "measure
// before writing ... rather than detecting overflow after").
func measuredLines(pdf *fpdf.Fpdf, w float64, text, style string, size float64) int {
	pdf.SetFont(fontFamily, style, size)
	if text == "" {
		return 0
	}
	return len(pdf.SplitText(text, w))
}

// panelWriter holds the running layout state for one problem's grading panel
// as it is drawn, paginating onto full-width continuation pages ("<label> —
// grading (continued)", no image half) whenever the next block would cross
// panelBottom. x/w are the current column's left edge and width — halfW on
// the problem's own first page-image page (paired with the image half), full
// content width on any continuation page this writer adds itself.
type panelWriter struct {
	pdf   *fpdf.Fpdf
	label string
	x, w  float64
	y     float64
}

// newPanelWriter starts a panel at the given top (the shared header
// baseline drawImageHalf also starts from), in the right-half column next to
// the problem's page image.
func newPanelWriter(pdf *fpdf.Fpdf, label string, top float64) *panelWriter {
	return &panelWriter{pdf: pdf, label: label, x: margin + halfW + gutter, w: halfW, y: top}
}

// ensureRoom spills onto a new full-width continuation page when the next
// block (blockH tall) would cross panelBottom — called before every block is
// drawn, per the fix brief's "measure before writing" requirement, so a
// panel never gets a chance to overflow the page it's on.
func (pw *panelWriter) ensureRoom(blockH float64) {
	if pw.y+blockH <= panelBottom {
		return
	}
	pw.pdf.AddPage()
	pw.x = margin
	pw.w = pageW - 2*margin
	pw.y = contentTop

	pw.pdf.SetXY(pw.x, pw.y)
	pw.pdf.SetFont(fontFamily, "B", panelFontSize+1)
	pw.pdf.MultiCell(pw.w, lineHeight, pw.label+" — grading (continued)", "", "L", false)
	pw.y = pw.pdf.GetY()
}

// criterionBlockHeight measures the total height (name line + optional
// comment + trailing spacer) one criterion's MultiCell calls will consume at
// the writer's current column width, matching drawCriterion's own drawing
// exactly so the "measure before writing" check is accurate.
func (pw *panelWriter) criterionBlockHeight(c CriterionLine) float64 {
	line := fmt.Sprintf("%s: %s/%s", c.Name, c.Score, c.Max)
	h := float64(measuredLines(pw.pdf, pw.w, line, "", criterionSize)) * lineHeight
	if c.Comment != "" {
		h += float64(measuredLines(pw.pdf, pw.w, c.Comment, "", criterionSize)) * lineHeight
	}
	return h + 1 // the pdf.Ln(1) spacer after each criterion
}

// drawCriterion draws one criterion's "name score/max" line plus its
// (grey) comment, if any, spilling to a continuation page first if it
// wouldn't otherwise fit (ensureRoom).
func (pw *panelWriter) drawCriterion(c CriterionLine) {
	pw.ensureRoom(pw.criterionBlockHeight(c))

	pw.pdf.SetXY(pw.x, pw.y)
	pw.pdf.SetFont(fontFamily, "", criterionSize)
	line := fmt.Sprintf("%s: %s/%s", c.Name, c.Score, c.Max)
	pw.pdf.MultiCell(pw.w, lineHeight, line, "", "L", false)
	if c.Comment != "" {
		pw.pdf.SetX(pw.x)
		pw.pdf.SetTextColor(90, 90, 90)
		pw.pdf.MultiCell(pw.w, lineHeight, c.Comment, "", "L", false)
		pw.pdf.SetTextColor(0, 0, 0)
	}
	pw.pdf.Ln(1)
	pw.y = pw.pdf.GetY()
}

// drawSubtotal draws the problem's "Subtotal: x / y" line, spilling to a
// continuation page first if needed — the fix brief's hard requirement is
// that "the problem subtotal must always land on-page", so this gets the
// same measure-first treatment as every criterion block.
func (pw *panelWriter) drawSubtotal(total, max string) {
	text := fmt.Sprintf("Subtotal: %s / %s", total, max)
	h := float64(measuredLines(pw.pdf, pw.w, text, "B", panelFontSize)) * lineHeight
	pw.ensureRoom(h)

	pw.pdf.SetXY(pw.x, pw.y)
	pw.pdf.SetFont(fontFamily, "B", panelFontSize)
	pw.pdf.MultiCell(pw.w, lineHeight, text, "", "L", false)
	pw.y = pw.pdf.GetY()
}

// drawPanel renders the full grading panel in the right half: problem
// label, each criterion's "name score/max" + comment, and the problem total
// (spec §3). When the content would run past the bottom of the page — many
// criteria, or long (e.g. Chinese) comments — it spills onto one or more
// full-width continuation pages instead of silently overflowing where
// pdfium would clip it (see panelBottom/ensureRoom).
func drawPanel(pdf *fpdf.Fpdf, p ProblemReport, top float64) {
	pw := newPanelWriter(pdf, p.Label, top)

	pdf.SetXY(pw.x, pw.y)
	pdf.SetFont(fontFamily, "B", panelFontSize+1)
	pdf.MultiCell(pw.w, lineHeight, p.Label, "", "L", false)
	pw.y = pdf.GetY()

	for _, c := range p.Criteria {
		pw.drawCriterion(c)
	}

	pw.drawSubtotal(p.Total, p.Max)
	pw.drawProblemComment(p.Comment)
}

// drawProblemComment draws the problem-level note under the subtotal — the
// same field the grade email's ProblemBreakdown discloses (D70: the PDF
// omitting it was a wiring gap), in the criterion-comment grey. Note that on
// this fpdf path any LaTeX in the comment stays raw source; only the Typst
// renderer typesets it.
func (pw *panelWriter) drawProblemComment(comment string) {
	if comment == "" {
		return
	}
	h := float64(measuredLines(pw.pdf, pw.w, comment, "", criterionSize)) * lineHeight
	pw.ensureRoom(h)

	pw.pdf.SetXY(pw.x, pw.y)
	pw.pdf.SetFont(fontFamily, "", criterionSize)
	pw.pdf.SetTextColor(90, 90, 90)
	pw.pdf.MultiCell(pw.w, lineHeight, comment, "", "L", false)
	pw.pdf.SetTextColor(0, 0, 0)
	pw.y = pw.pdf.GetY()
}

// drawContinuedPanel renders the abbreviated panel for a problem's later
// pages (spec §3: "later pages show \"(continued)\"") — the full grading
// detail already appeared on the problem's first page, so repeating it would
// just be noise.
func drawContinuedPanel(pdf *fpdf.Fpdf, p ProblemReport, top float64) {
	x := margin + halfW + gutter
	w := halfW
	pdf.SetXY(x, top)
	pdf.SetFont(fontFamily, "B", panelFontSize+1)
	pdf.MultiCell(w, lineHeight, p.Label, "", "L", false)
	pdf.SetX(x)
	pdf.SetFont(fontFamily, "", continuedSize)
	pdf.SetTextColor(120, 120, 120)
	pdf.MultiCell(w, lineHeight, "(continued)", "", "L", false)
	pdf.SetTextColor(0, 0, 0)
}

// downscaleForReport applies the "compressed" quality option's fixed
// downscale (spec §3, D44): long edge 1600px, JPEG q75.
func downscaleForReport(pageJPEG []byte) ([]byte, error) {
	return imaging.DownscaleLongEdge(pageJPEG, compressedLongEdgePx, compressedJPEGQuality)
}

// sizeGuardBytes is the 15MB per-item attachment size the publish batch view
// warns on (spec §3: "Size guard"). Exported as a named constant here (not
// buried in the publish package) so the layout/build code and any caller
// checking against it agree on one number.
const sizeGuardBytes = 15 * 1024 * 1024

// ExceedsSizeGuard reports whether built attachment bytes exceed the 15MB
// per-item warning threshold (spec §3). It does not fail the build — send
// still proceeds; this is purely informational for the caller's warning UI.
func ExceedsSizeGuard(attachmentBytes []byte) bool {
	return len(attachmentBytes) > sizeGuardBytes
}

// pageLabel is a small helper kept for the ZIP builder's filename convention
// (problem-<n>-page-<m>.jpg) — n/m are both 1-based per spec §3.
func pageLabel(problemIdx, pageIdx int) string {
	return "problem-" + strconv.Itoa(problemIdx+1) + "-page-" + strconv.Itoa(pageIdx+1) + ".jpg"
}
