package ingest

import (
	"os"
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// TestIngest_FlagsTextRenderLoss is the end-to-end wiring regression for the
// pdfium CJK text-loss bug: ingesting a real typeset PDF whose header carries a
// non-embedded CID-font student name (data/demo/demo-submissions) must stamp
// text_loss_runs on the created answer pages, which is what feeds the
// text_render_loss workflow warning (CountTextRenderLossPages). Uses the REAL
// renderer — the fake zero-reports by design.
func TestIngest_FlagsTextRenderLoss(t *testing.T) {
	pdf, err := os.ReadFile("../../data/demo/demo-submissions/B11902001.pdf")
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}

	f := setup(t)
	// The demo paper has four pages. Keep this whole-assessment fixture's
	// positional contract valid: one problem per PDF page. This test used to rely
	// on ingest silently dropping the final two pages, which is no longer safe.
	for i := 3; i <= 4; i++ {
		maxPoints, _ := store.Num("10")
		if _, err := f.st.Q.CreateProblem(f.ctx, db.CreateProblemParams{
			AssessmentID: f.aid, Number: int32(i), MaxPoints: maxPoints, Position: int32(i),
		}); err != nil {
			t.Fatalf("create problem %d: %v", i, err)
		}
	}
	r, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("pdfium: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	f.svc.Renderer = r

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", pdf, 0, false)
	if res.Status != "ingested" || res.MappedPages == 0 {
		t.Fatalf("ingest: %+v", res)
	}

	pages, err := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if err != nil || len(pages) == 0 {
		t.Fatalf("pages: got %d (%v)", len(pages), err)
	}
	flagged := 0
	for _, p := range pages {
		if p.TextLossRuns > 0 {
			flagged++
		}
	}
	// Every mapped page carries the CJK name box that pdfium drops.
	if flagged != len(pages) {
		t.Fatalf("text_loss_runs: %d of %d pages flagged, want all", flagged, len(pages))
	}

	n, err := f.st.Q.CountTextRenderLossPages(f.ctx, f.aid)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(n) != len(pages) {
		t.Fatalf("CountTextRenderLossPages = %d, want %d", n, len(pages))
	}
}
