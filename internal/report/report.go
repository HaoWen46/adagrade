// Package report builds the per-student result PDF and its ZIP-of-images
// fallback attached to grade emails (spec §3, D42). The PDF layout ("kinda
// like some evaluation stuff") lands in students' inboxes, so it gets real
// design attention: A4 landscape, left half the student's own page image,
// right half the grading panel, "(continued)" markers on multi-page answers.
//
// This package is DB-free and pure: it takes already-fetched page JPEG bytes
// (the caller — the publish send job — resolves blobs) and returns built
// bytes. It never touches the database, the queue, or a live font download;
// the font file itself is a `make report-fonts` artifact the caller points
// ADAMARKER_REPORT_FONT at (internal/config.Config.ReportFontPath).
//
// PII rule (CLAUDE.md): the built PDF/ZIP bytes themselves carry the
// student's page images and grading comments — real PII once populated with
// real data — but this package's own log/error strings never include field
// content (student names, comments), only structural detail (page counts,
// byte sizes).
package report

import (
	"bytes"
	"fmt"

	"github.com/HaoWen46/adagrade/internal/email"
)

// Quality selects how page images are encoded into the attachment (spec §3,
// D44). There are exactly three values app-wide; "none" never reaches this
// package (the publish layer skips building an attachment entirely).
const (
	QualityCompressed = "compressed"
	QualityOriginal   = "original"
)

// compressedLongEdgePx and compressedJPEGQuality are the "compressed" option's
// fixed downscale target (spec §3, D44: "long edge 1600px, JPEG q75").
const (
	compressedLongEdgePx  = 1600
	compressedJPEGQuality = 75
)

// CriterionLine is re-exported from internal/email so callers building a
// ReportInput from the same publish snapshot that feeds RenderGradeEmail can
// share one type — the PDF's grading panel and the email body describe the
// exact same breakdown.
type CriterionLine = email.CriterionLine

// ProblemReport is one problem's worth of content in the PDF/ZIP: its answer
// page images (in order, possibly spanning several PDF pages) and its
// grading panel (criteria + total).
type ProblemReport struct {
	Label    string   // e.g. "Problem 3: Green's theorem"
	Pages    [][]byte // JPEG bytes, one per answer page, caller-fetched from blobs (ORIGINAL/unmasked — students see their own work, spec §3)
	Criteria []CriterionLine
	Total    string // decimal string (never float64, CLAUDE.md)
	Max      string
	// Comment is the problem-level note (grader's overall comment) — the same
	// field the grade email's ProblemBreakdown already discloses to the
	// student; per-criterion rationales stay out of student output on both
	// surfaces (typst-report spec 2026-07-20).
	Comment string
}

// ReportInput is everything Build/BuildZIP need to produce one student's
// attachment. It carries no recipient email address — the caller (send job)
// already has that on domain.OutboundEmail.
type ReportInput struct {
	AssessmentName string
	StudentName    string
	StudentID      string
	Problems       []ProblemReport
	Quality        string // "compressed" | "original" — never "none" here (caller's job to skip Build entirely)
	Total, Max     string // assessment-level total, shown on the header (spec §3: "first page ... carries a header ... assessment total")
}

// validate checks the fields Build/BuildZIP both depend on, so a caller
// mistake (typo'd Quality, no font configured) fails loudly at the seam
// rather than surfacing as a malformed PDF.
func (in ReportInput) validate() error {
	switch in.Quality {
	case QualityCompressed, QualityOriginal:
	default:
		return fmt.Errorf("report: invalid Quality %q (want %q|%q)", in.Quality, QualityCompressed, QualityOriginal)
	}
	return nil
}

// resolvePageJPEG returns the page bytes for the given quality: downscaled
// for "compressed", verbatim for "original" (spec §3: "page images as
// stored").
func resolvePageJPEG(pageJPEG []byte, quality string) ([]byte, error) {
	if quality == QualityCompressed {
		out, err := downscaleForReport(pageJPEG)
		if err != nil {
			return nil, fmt.Errorf("report: downscale page image: %w", err)
		}
		return out, nil
	}
	return pageJPEG, nil
}

// Build renders the merged per-student result PDF: A4 landscape, one page per
// answer image with the grading panel alongside (spec §3 layout — see
// layout.go for the actual drawing code). fontPath must be a UTF-8 TTF
// (Noto Sans TC, `make report-fonts`); Build does not consult config or the
// environment itself — the caller decides whether the feature is enabled
// (config.ReportFontConfigured) and passes the resolved path.
func Build(fontPath string, in ReportInput) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if fontPath == "" {
		return nil, fmt.Errorf("report: Build requires a non-empty fontPath (ADAMARKER_REPORT_FONT) — the caller must gate on config.ReportFontConfigured before calling Build")
	}

	pdf, err := newDoc(fontPath)
	if err != nil {
		return nil, err
	}

	if err := drawReport(pdf, in); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("report: render PDF: %w", err)
	}
	return buf.Bytes(), nil
}

// CheckFont validates that fontPath points at a font fpdf can actually load
// (readable file, TrueType glyf-outline format — fpdf rejects CFF-based
// OTF/OTTO fonts, see layout.go's newDoc) without building a full report. It
// is the startup-time check (mirroring internal/localocr's "construction
// failed" warning pattern): main.go calls this once when
// config.ReportFontConfigured() is true so a misconfigured
// ADAMARKER_REPORT_FONT is a loud warning at boot, not a silent failure the
// first time a TA tries to publish with attachments.
func CheckFont(fontPath string) error {
	pdf, err := newDoc(fontPath)
	if err != nil {
		return err
	}
	// newDoc already surfaces load errors via pdf.Error(), but AddPage+Output
	// forces fpdf to actually walk the font's glyph tables (glyf/loca/cmap)
	// rather than just opening the file, catching a truncated-but-readable
	// font that newDoc's checks alone would miss.
	pdf.AddPage()
	pdf.SetFont(fontFamily, "", panelFontSize)
	pdf.Cell(10, 10, "ok")
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return fmt.Errorf("report: font %q failed a trial render: %w", fontPath, err)
	}
	return nil
}
