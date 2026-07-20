package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
)

// loadDemoPile loads the committed demo fixture whose page 1 reproduces the
// verified bug: the name box text 米樂水 (non-embedded CJK font) is present in
// the text layer but renders as blank pixels, while the Helvetica ID box
// B11902008 on the same page renders fine.
func loadDemoPile(t *testing.T) []byte {
	t.Helper()
	pdf, err := os.ReadFile("../../data/demo/demo-scan-pile.pdf")
	if err != nil {
		t.Skipf("demo fixture unavailable: %v", err)
	}
	return pdf
}

// textPDF builds a one-page PDF drawing text in base-14 Helvetica — a font
// PDFium always renders — so every text-layer char has ink (the clean case).
func textPDF(t *testing.T, text string) []byte {
	t.Helper()
	var body bytes.Buffer
	offsets := []int{0} // object 0 is the free head
	write := func(s string) {
		body.WriteString(s)
	}
	addObj := func(s string) {
		offsets = append(offsets, body.Len())
		write(s)
	}

	write("%PDF-1.4\n")
	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
	addObj("1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n")
	addObj("2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n")
	addObj("3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >> endobj\n")
	addObj(fmt.Sprintf("4 0 obj << /Length %d >> stream\n%s\nendstream endobj\n", len(content), content))
	addObj("5 0 obj << /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >> endobj\n")

	xrefAt := body.Len()
	write(fmt.Sprintf("xref\n0 %d\n", len(offsets)))
	write("0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		write(fmt.Sprintf("%010d 00000 n \n", off))
	}
	write(fmt.Sprintf("trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefAt))
	return body.Bytes()
}

// TestProbeTextLoss_FixtureFlagsLostCJKName is the live-bug regression test:
// page 1 of the demo pile must be flagged, and the flag must be LOCALIZED to
// the lost name run — the Helvetica ID box (B11902008) and the answer body
// render fine and must not be counted, so exactly one suspect run remains and
// the sample is exactly the three lost name chars.
func TestProbeTextLoss_FixtureFlagsLostCJKName(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	doc, err := r.Open(ctx, loadDemoPile(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	// Probe at two different raster scales to pin the points→pixels math:
	// the verdict must not depend on the DPI the page happened to render at.
	for _, opts := range []Options{
		{DPI: 150, MaxLongEdgePx: 2200, JPEGQuality: 85},
		{DPI: 75, MaxLongEdgePx: 2200, JPEGQuality: 85},
	} {
		img, _, err := doc.RenderPageImage(ctx, 0, opts)
		if err != nil {
			t.Fatalf("RenderPageImage (dpi %d): %v", opts.DPI, err)
		}
		rep, err := ProbeTextLoss(ctx, doc, 0, img)
		if err != nil {
			t.Fatalf("ProbeTextLoss (dpi %d): %v", opts.DPI, err)
		}
		if !rep.HasTextLayer {
			t.Errorf("dpi %d: HasTextLayer = false, want true (pypdf extracts the text)", opts.DPI)
		}
		if rep.SuspectRuns != 1 {
			t.Errorf("dpi %d: SuspectRuns = %d, want exactly 1 (the lost name box, nothing else)", opts.DPI, rep.SuspectRuns)
		}
		if rep.SampleText != "米樂水" {
			t.Errorf("dpi %d: SampleText = %q, want the three lost name chars only", opts.DPI, rep.SampleText)
		}
	}
}

// TestProbeTextLoss_CleanPageNotFlagged is the negative case: a page whose
// entire text layer renders (base-14 Helvetica, mixed case + punctuation to
// exercise thin glyphs) must report zero suspect runs.
func TestProbeTextLoss_CleanPageNotFlagged(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	doc, err := r.Open(ctx, textPDF(t, "Hello, World 123. All of this renders fine."))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	img, _, err := doc.RenderPageImage(ctx, 0, Options{DPI: 150, MaxLongEdgePx: 2200, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("RenderPageImage: %v", err)
	}
	rep, err := ProbeTextLoss(ctx, doc, 0, img)
	if err != nil {
		t.Fatalf("ProbeTextLoss: %v", err)
	}
	if !rep.HasTextLayer {
		t.Error("HasTextLayer = false, want true")
	}
	if rep.SuspectRuns != 0 {
		t.Errorf("SuspectRuns = %d, want 0 (everything renders)", rep.SuspectRuns)
	}
	if rep.SampleText != "" {
		t.Errorf("SampleText = %q, want empty on a clean page", rep.SampleText)
	}
}

// TestProbeTextLoss_NoTextLayer: a blank page (and, in production, a scanner
// bitmap page) has no text layer — nothing to lose, nothing to flag.
func TestProbeTextLoss_NoTextLayer(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	doc, err := r.Open(ctx, minimalPDF(t, 1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	img, _, err := doc.RenderPageImage(ctx, 0, Options{DPI: 100, MaxLongEdgePx: 2200, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("RenderPageImage: %v", err)
	}
	rep, err := ProbeTextLoss(ctx, doc, 0, img)
	if err != nil {
		t.Fatalf("ProbeTextLoss: %v", err)
	}
	if rep.HasTextLayer || rep.SuspectRuns != 0 || rep.SampleText != "" {
		t.Errorf("blank page: got %+v, want zero report", rep)
	}
}

// TestProbeTextLoss_OutOfRangePageErrors: a bad page index must surface as an
// error, not a silent zero report.
func TestProbeTextLoss_OutOfRangePageErrors(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	doc, err := r.Open(ctx, minimalPDF(t, 1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	img, _, err := doc.RenderPageImage(ctx, 0, Options{DPI: 100, MaxLongEdgePx: 2200, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("RenderPageImage: %v", err)
	}
	if _, err := ProbeTextLoss(ctx, doc, 5, img); err == nil {
		t.Error("out-of-range page index should error")
	}
}

// BenchmarkProbeTextLoss pins the "cheap per page" requirement: the probe
// (text extraction + box pixel sampling) must cost a small fraction of the
// page render it accompanies. Run with -bench=ProbeTextLoss.
func BenchmarkProbeTextLoss(b *testing.B) {
	pdf, err := os.ReadFile("../../data/demo/demo-scan-pile.pdf")
	if err != nil {
		b.Skipf("demo fixture unavailable: %v", err)
	}
	r, err := NewPDFium()
	if err != nil {
		b.Fatalf("init pdfium: %v", err)
	}
	defer func() { _ = r.Close() }()
	ctx := context.Background()
	doc, err := r.Open(ctx, pdf)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()
	img, _, err := doc.RenderPageImage(ctx, 0, Options{DPI: 250, MaxLongEdgePx: 2200, JPEGQuality: 85})
	if err != nil {
		b.Fatalf("RenderPageImage: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ProbeTextLoss(ctx, doc, 0, img); err != nil {
			b.Fatal(err)
		}
	}
}

// TestProbeTextLoss_FakeReturnsZeroReport: the Fake's documents support the
// probe seam (so callers can be written once against ProbeTextLoss) and always
// report a clean page.
func TestProbeTextLoss_FakeReturnsZeroReport(t *testing.T) {
	f := NewFake(1)
	ctx := context.Background()
	doc, err := f.Open(ctx, []byte("x"))
	if err != nil {
		t.Fatalf("fake Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	img, _, err := doc.RenderPageImage(ctx, 0, Options{})
	if err != nil {
		t.Fatalf("fake RenderPageImage: %v", err)
	}
	rep, err := ProbeTextLoss(ctx, doc, 0, img)
	if err != nil {
		t.Fatalf("fake ProbeTextLoss: %v", err)
	}
	if rep != (TextLossReport{}) {
		t.Errorf("fake probe: got %+v, want zero report", rep)
	}
}
