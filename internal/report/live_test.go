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
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
)

// TestLive_CJKCommentRenders is an opt-in test against the REAL configured
// report font (Noto Sans TC, `make report-fonts`), skipped unless
// ADAMARKER_REPORT_FONT is set — mirrors internal/localocr/live_test.go's
// gating for the ONNX assets. Go Regular (used by the rest of this package's
// tests) has no CJK glyph coverage, so it cannot prove CJK actually renders;
// this test is the one place that matters.
//
// It builds a report whose criterion comment contains "中文字" (Chinese
// characters), rasterizes the PDF back via internal/render, and asserts the
// right-half grading panel — where that comment is drawn — has visible glyph
// ink. A synthetic fixture only (invented name/ID, invented comment text) —
// no student PII per CLAUDE.md.
func TestLive_CJKCommentRenders(t *testing.T) {
	fontPath := os.Getenv("ADAMARKER_REPORT_FONT")
	if fontPath == "" {
		t.Skip("live CJK report test skipped: set ADAMARKER_REPORT_FONT to run (see `make report-fonts`)")
	}

	in := ReportInput{
		AssessmentName: "期中考 2",
		StudentName:    "王小明",
		StudentID:      "B11902888",
		Total:          "18",
		Max:            "20",
		Quality:        QualityOriginal,
		Problems: []ProblemReport{
			{
				Label: "第一題",
				Pages: [][]byte{cjkTestPageJPEG(t)},
				Criteria: []CriterionLine{
					{Name: "正確性", Score: "9", Max: "10", Comment: "這裡有中文字的評語，缺少一個步驟。"},
				},
				Total: "9",
				Max:   "10",
			},
		},
	}

	out, err := Build(fontPath, in)
	if err != nil {
		t.Fatalf("Build with real ADAMARKER_REPORT_FONT: %v", err)
	}

	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium renderer: %v", err)
	}
	defer func() { _ = r.Close() }()

	page, err := r.RenderPage(context.Background(), out, 0, render.Options{DPI: 150, MaxLongEdgePx: 6000, JPEGQuality: 90})
	if err != nil {
		t.Fatalf("render first page: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(page.JPEG))
	if err != nil {
		t.Fatalf("decode rendered page: %v", err)
	}

	rightFrac := inkFraction(img, rightHalfRect(img.Bounds()))
	if rightFrac < 0.001 {
		t.Errorf("right half (grading panel with CJK comment) ink fraction = %.5f, want > 0.001 — the CJK comment glyphs should be visible", rightFrac)
	}
}

// TestLive_LongCJKCommentPanelPaginates is the CJK counterpart of
// TestBuild_TallPanelSpillsToContinuationPageAndKeepsSubtotal: the ASCII
// fixture proves the pagination logic works in the ungated suite, but only
// real CJK text through the real font proves the finding's actual trigger
// (long Chinese-language comments, which wrap very differently from ASCII —
// isChinese() in fpdf's SplitText breaks after every CJK character rather
// than at spaces) doesn't silently clip the panel's bottom criteria or its
// subtotal.
func TestLive_LongCJKCommentPanelPaginates(t *testing.T) {
	fontPath := os.Getenv("ADAMARKER_REPORT_FONT")
	if fontPath == "" {
		t.Skip("live CJK report test skipped: set ADAMARKER_REPORT_FONT to run (see `make report-fonts`)")
	}

	const longCJKComment = "這是一段刻意寫得很長的中文評語，目的是讓它在評分欄位的窄欄寬度中換行成好幾行，" +
		"用來測試當有很多評分項目、且每一項都附上很長的中文評語時，評分面板是否會超出頁面底部而被裁切。" +
		"這裡再多寫一些內容，確保換行的行數足夠多，能夠真正撐滿甚至超過單一頁面的可用高度。"

	criteria := make([]CriterionLine, 12)
	for i := range criteria {
		criteria[i] = CriterionLine{
			Name:    fmt.Sprintf("評分項目 %d", i+1),
			Score:   "4",
			Max:     "5",
			Comment: longCJKComment,
		}
	}

	in := ReportInput{
		AssessmentName: "期中考 2",
		StudentName:    "王小明",
		StudentID:      "B11902888",
		Total:          "18",
		Max:            "20",
		Quality:        QualityOriginal,
		Problems: []ProblemReport{
			{
				Label:    "第一題：許多評分項目",
				Pages:    [][]byte{cjkTestPageJPEG(t)},
				Criteria: criteria,
				Total:    "48",
				Max:      "60",
			},
		},
	}

	out, err := Build(fontPath, in)
	if err != nil {
		t.Fatalf("Build with real ADAMARKER_REPORT_FONT: %v", err)
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
		t.Fatalf("page count = %d, want >= 2 — 12 long-CJK-comment criteria must spill onto a continuation page instead of being clipped", pageCount)
	}

	foundSubtotalInk := false
	for i := 0; i < pageCount; i++ {
		page, err := r.RenderPage(context.Background(), out, i, render.Options{DPI: 100, MaxLongEdgePx: 4000, JPEGQuality: 85})
		if err != nil {
			t.Fatalf("render page %d: %v", i, err)
		}
		img, err := jpeg.Decode(bytes.NewReader(page.JPEG))
		if err != nil {
			t.Fatalf("decode rendered page %d: %v", i, err)
		}
		b := img.Bounds()
		bandTop := b.Min.Y + int(float64(b.Dy())*0.5)
		band := image.Rect(b.Min.X, bandTop, b.Max.X, b.Max.Y)
		if inkFraction(img, band) > 0.0005 {
			foundSubtotalInk = true
			break
		}
	}
	if !foundSubtotalInk {
		t.Error("no page's bottom half shows ink where the CJK panel's Subtotal line should land — it was likely clipped off the bottom of a page")
	}
}

// cjkTestPageJPEG is a plain white "page image" — the CJK test cares about
// the grading-panel text (which is what exercises the font), not the left
// half's image content, so a blank page is fine here.
func cjkTestPageJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 600))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test page JPEG: %v", err)
	}
	return buf.Bytes()
}
