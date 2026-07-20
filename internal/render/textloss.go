// Text-loss probe: detects silent glyph loss in rasterized pages.
//
// The WASM PDFium build ships no system fonts, so a page that references a
// non-embedded CID/CJK font keeps its text layer (FPDFText_* still extracts
// the characters) but renders those glyphs as nothing — the AI then grades an
// image with the text missing, and nothing fails. Real scanner output is
// bitmap-only (no text layer) and immune; student-submitted typeset PDFs are
// the exposed path. The probe cross-checks the two channels: for each
// text-layer character it asks whether the rendered raster has ANY ink inside
// that character's box. A run of >= minLostRunLen visible characters with
// text-layer content but blank pixels is probable glyph loss.
//
// Known limits (heuristic by design, biased against false flags):
//   - pages with /Rotate or a non-white background can hide loss (chars judged
//     "unknown"/"inked"), never invent it on ordinary pages;
//   - a lost char whose box straddles a form-field border or underline samples
//     that ruling as ink and is missed — the surrounding run usually still trips.
package render

import (
	"context"
	"fmt"
	"image"
	"math"
	"unicode"

	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
)

// TextLossReport summarizes probable glyph loss on one rendered page.
type TextLossReport struct {
	// HasTextLayer is true when the page has at least one visible text-layer
	// character (scanner bitmap pages have none and can never lose text).
	HasTextLayer bool
	// SuspectRuns counts maximal runs of >= minLostRunLen visible characters
	// that exist in the text layer but left no ink in the raster. 0 = clean.
	SuspectRuns int
	// SampleText holds the first sampleTextCap lost characters, for server
	// logs and debugging ONLY — it is likely a student name or answer content
	// (PII) and MUST NOT appear in user-facing output (CLAUDE.md privacy rule).
	SampleText string
}

// TextLossProber is the optional capability a Document may implement. It is
// deliberately not part of Document so implementations owned elsewhere (e.g.
// test doubles in internal/scan) keep compiling unchanged.
type TextLossProber interface {
	// ProbeTextLoss cross-checks pageIndex's text layer against raster, the
	// image RenderPageImage returned for that same page. The raster's pixel
	// dimensions define the page-points→pixels scale, so any DPI/long-edge
	// combination works as long as raster came from this page.
	ProbeTextLoss(ctx context.Context, pageIndex int, raster image.Image) (TextLossReport, error)
}

// ProbeTextLoss probes doc's page if the implementation supports it and
// returns a zero (clean) report otherwise, so callers need no type assertions.
func ProbeTextLoss(ctx context.Context, doc Document, pageIndex int, raster image.Image) (TextLossReport, error) {
	if p, ok := doc.(TextLossProber); ok {
		return p.ProbeTextLoss(ctx, pageIndex, raster)
	}
	return TextLossReport{}, nil
}

const (
	// minLostRunLen: a run of at least this many consecutive blank visible
	// chars is flagged. Below it, thin punctuation or an isolated odd glyph
	// could false-flag; a lost name/word is at least this long.
	minLostRunLen = 3
	// sampleTextCap bounds TextLossReport.SampleText (runes).
	sampleTextCap = 40
	// inkThreshold8/16 split "background" from "ink": a pixel is inked when
	// any channel drops below ~87.5% white, tolerant of JPEG-free AA fringes
	// (PDFium rasterizes on a white background).
	inkThreshold8  = 0xE0
	inkThreshold16 = 0xE000
)

// charInkVerdict is the tri-state outcome of sampling one character's boxes.
type charInkVerdict int

const (
	inkPresent charInkVerdict = iota // glyph rendered — breaks any blank run
	inkAbsent                        // text-layer char with blank pixels — extends the run
	inkUnknown                       // cannot judge (degenerate boxes, API error) — neutral
)

// ProbeTextLoss implements TextLossProber on the PDFium document. Cost is one
// text-page load plus 2-3 cheap in-process WASM calls per character and pixel
// sampling over character boxes only (with early exit on the first inked
// pixel), so it is a small fraction of the page render it accompanies.
func (d *pdfiumDoc) ProbeTextLoss(ctx context.Context, pageIndex int, raster image.Image) (TextLossReport, error) {
	if d.closed {
		return TextLossReport{}, fmt.Errorf("render: text-loss probe on closed document")
	}
	if raster == nil {
		return TextLossReport{}, fmt.Errorf("render: text-loss probe requires the rendered raster")
	}
	pageRef := requests.Page{ByIndex: &requests.PageByIndex{Document: d.doc, Index: pageIndex}}

	// The raster's pixel size over the page's point size is the exact scale
	// RenderPage used (page stretched to the computed pixel box), giving the
	// points→pixels map for char boxes. PDF y grows upward, raster y downward.
	size, err := d.inst.GetPageSize(&requests.GetPageSize{Page: pageRef})
	if err != nil {
		return TextLossReport{}, fmt.Errorf("render: probe page size (index %d): %w", pageIndex, err)
	}
	if size.Width <= 0 || size.Height <= 0 {
		return TextLossReport{}, fmt.Errorf("render: probe degenerate page size %.1fx%.1fpt", size.Width, size.Height)
	}
	bounds := raster.Bounds()
	scaleX := float64(bounds.Dx()) / size.Width
	scaleY := float64(bounds.Dy()) / size.Height

	tp, err := d.inst.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: pageRef})
	if err != nil {
		return TextLossReport{}, fmt.Errorf("render: probe text page (index %d): %w", pageIndex, err)
	}
	defer func() {
		_, _ = d.inst.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: tp.TextPage})
	}()
	count, err := d.inst.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: tp.TextPage})
	if err != nil {
		return TextLossReport{}, fmt.Errorf("render: probe char count (index %d): %w", pageIndex, err)
	}

	var rep TextLossReport
	var sample []rune
	var run []rune // blank visible chars in the current maximal run
	flush := func() {
		if len(run) >= minLostRunLen {
			rep.SuspectRuns++
			for _, r := range run {
				if len(sample) < sampleTextCap {
					sample = append(sample, r)
				}
			}
		}
		run = run[:0]
	}
	for i := 0; i < count.Count; i++ {
		u, err := d.inst.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: tp.TextPage, Index: i})
		if err != nil {
			continue // unreadable char: neutral
		}
		r := rune(u.Unicode)
		if !isVisibleTextRune(r) {
			// Whitespace legitimately has no ink: it never extends a run, and
			// only a line break bounds one (a lost run may span word gaps —
			// "米 樂 水" is one loss, not three sub-threshold ones).
			if r == '\n' || r == '\r' {
				flush()
			}
			continue
		}
		rep.HasTextLayer = true
		switch d.charInk(tp.TextPage, i, raster, scaleX, scaleY) {
		case inkPresent:
			flush()
		case inkAbsent:
			run = append(run, r)
		case inkUnknown:
			// neutral: neither evidence of loss nor of rendering
		}
	}
	flush()
	rep.SampleText = string(sample)
	return rep, nil
}

// charInk samples the raster inside one character's boxes. Two-step to stay
// bias-safe: the tight glyph box of a rendered char always contains ink (±1px
// antialiasing pad), while a lost glyph leaves a degenerate/blank tight box —
// only then consult the loose (advance/em) box, inset 1px so a neighbouring
// glyph's antialiasing fringe cannot masquerade as this char's ink.
func (d *pdfiumDoc) charInk(tp references.FPDF_TEXTPAGE, index int, raster image.Image, scaleX, scaleY float64) charInkVerdict {
	bounds := raster.Bounds()
	tight, err := d.inst.FPDFText_GetCharBox(&requests.FPDFText_GetCharBox{TextPage: tp, Index: index})
	if err == nil {
		r := deviceRect(tight.Left, tight.Bottom, tight.Right, tight.Top, scaleX, scaleY, bounds)
		if r.Dx() >= 1 && r.Dy() >= 1 && inkAny(raster, r.Inset(-1)) {
			return inkPresent
		}
	}
	loose, err := d.inst.FPDFText_GetLooseCharBox(&requests.FPDFText_GetLooseCharBox{TextPage: tp, Index: index})
	if err != nil {
		return inkUnknown
	}
	r := deviceRect(float64(loose.Rect.Left), float64(loose.Rect.Bottom), float64(loose.Rect.Right), float64(loose.Rect.Top), scaleX, scaleY, bounds).Inset(1)
	if r.Empty() {
		return inkUnknown // zero-advance char (combining mark etc.) — cannot judge
	}
	if inkAny(raster, r) {
		return inkPresent
	}
	return inkAbsent
}

// deviceRect maps a PDF-page-space box (points, y up) to raster pixel space
// (y down), outward-rounded so partial pixels stay inside the sample region.
func deviceRect(left, bottom, right, top, scaleX, scaleY float64, bounds image.Rectangle) image.Rectangle {
	h := float64(bounds.Dy())
	x0 := bounds.Min.X + int(math.Floor(left*scaleX))
	x1 := bounds.Min.X + int(math.Ceil(right*scaleX))
	y0 := bounds.Min.Y + int(math.Floor(h-top*scaleY))
	y1 := bounds.Min.Y + int(math.Ceil(h-bottom*scaleY))
	return image.Rect(x0, y0, x1, y1) // canonicalizes if a box arrives inverted
}

// inkAny reports whether any pixel of img inside r (clamped to img's bounds)
// is darker than the background threshold, exiting on the first hit. The
// *image.RGBA fast path covers the raster RenderPageImage returns.
func inkAny(img image.Image, r image.Rectangle) bool {
	r = r.Intersect(img.Bounds())
	if r.Empty() {
		return false
	}
	if rgba, ok := img.(*image.RGBA); ok {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := rgba.Pix[rgba.PixOffset(r.Min.X, y):rgba.PixOffset(r.Max.X, y)]
			for x := 0; x < len(row); x += 4 {
				if row[x] < inkThreshold8 || row[x+1] < inkThreshold8 || row[x+2] < inkThreshold8 {
					return true
				}
			}
		}
		return false
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			if cr < inkThreshold16 || cg < inkThreshold16 || cb < inkThreshold16 {
				return true
			}
		}
	}
	return false
}

// isVisibleTextRune: chars expected to leave ink. Whitespace and control chars
// are legitimately blank; U+0000 (no ToUnicode mapping) still occupies layout
// space, so it counts as visible.
func isVisibleTextRune(r rune) bool {
	if r == 0 {
		return true
	}
	return !unicode.IsSpace(r) && !unicode.IsControl(r)
}
