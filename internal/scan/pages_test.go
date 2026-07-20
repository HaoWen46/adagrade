package scan

import (
	"context"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// looseSource creates a batch with one loose source and returns the source row.
func looseSource(f fx, t *testing.T, filename, content string, nb NewBatch) db.ScanSource {
	t.Helper()
	view, err := f.svc.CreateBatch(f.ctx, f.aid, nb, []SourceUpload{
		{Filename: filename, R: strings.NewReader(content)},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources: %d, %v", len(srcs), err)
	}
	return srcs[0]
}

func TestSplitSource_PDFCreatesPageRowsAndChunks(t *testing.T) {
	f := setup(t) // fake renderer: 3 pages
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1 three pages", NewBatch{})

	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages = %d (%v), want 3", len(pages), err)
	}
	if len(*f.renders) != 1 || len((*f.renders)[0].PageIDs) != 3 {
		t.Fatalf("render chunks = %+v, want one chunk of 3", *f.renders)
	}
	// idempotent under redelivery
	*f.renders = nil
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, _ = f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if len(pages) != 3 {
		t.Fatalf("pages after redelivery = %d, want 3", len(pages))
	}
}

func TestSplitSource_ImageIsOnePage(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "page.png", string(pngBytes(t, 40, 60)), NewBatch{})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, _ := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
}

func TestRenderPages_StoresImageAndThreeCrops(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, _ := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	for _, p := range pages {
		if !p.ImageRef.Valid || !p.StudentIDCropRef.Valid || !p.NameCropRef.Valid || !p.ProblemCropRef.Valid {
			t.Fatalf("page %d missing render outputs: %+v", p.ID, p)
		}
		if !strings.Contains(p.StudentIDCropRef.String, "/idcrop/") {
			t.Fatalf("crop key must contain /idcrop/: %s", p.StudentIDCropRef.String)
		}
		if !p.ImageSha256.Valid || p.ImageSha256.String == "" {
			t.Fatalf("page %d missing image sha", p.ID)
		}
	}
	if len(*f.identifies) != len(pages) {
		t.Fatalf("identify enqueues = %d, want %d", len(*f.identifies), len(pages))
	}
}

func TestRenderPages_OCRDisabledNoLocal_NoIdentify(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: false})
	_ = f.svc.SplitSource(f.ctx, src.ID)
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	if len(*f.identifies) != 0 {
		t.Fatalf("identify enqueues = %d, want 0", len(*f.identifies))
	}
}

func TestRenderPages_OCRDisabledNoLocal_PagesBecomeOrphans(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: false})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	if len(*f.identifies) != 0 {
		t.Fatalf("identify enqueues = %d, want 0 (OCR off)", len(*f.identifies))
	}
	pages, _ := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	for _, p := range pages {
		// With no OCR rung to run, the page's identification is complete
		// (vacuously): it must surface as an orphan for manual assignment,
		// not sit in "processing" forever.
		if !p.IdentifiedAt.Valid {
			t.Fatalf("page %d not marked identified; would be stuck processing", p.ID)
		}
		if p.OcrStudentIDLegible.Valid && p.OcrStudentIDLegible.Bool {
			t.Fatalf("page %d should carry illegible/empty reads", p.ID)
		}
	}
}

func TestRenderPages_RedeliveryReenqueuesUnidentified(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	first := len(*f.identifies)
	if first == 0 {
		t.Fatal("first render enqueued no identifies")
	}
	// Redelivery: pages are already rendered but not yet identified —
	// identify must be re-enqueued or a mid-chunk crash strands them.
	*f.identifies = (*f.identifies)[:0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	if len(*f.identifies) != first {
		t.Fatalf("redelivery enqueued %d identifies, want %d", len(*f.identifies), first)
	}
}

func TestSplitSource_RerunIncludesRenderedUnidentified(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	// SplitSource redelivery after render, before identify: the rendered-but-
	// unidentified pages must still be chunked so RenderPages can re-enqueue
	// their identify jobs.
	*f.renders = (*f.renders)[:0]
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if len(*f.renders) != 1 || len((*f.renders)[0].PageIDs) != len(chunk.PageIDs) {
		t.Fatalf("split rerun chunks = %+v, want the %d rendered-unidentified pages re-chunked", *f.renders, len(chunk.PageIDs))
	}
}

// failOpenRenderer is a stub Renderer whose Open always fails with a fixed
// error, so tests can exercise the open-failure path without a real PDF.
type failOpenRenderer struct {
	render.Renderer
	err error
}

func (r failOpenRenderer) Open(ctx context.Context, pdf []byte) (render.Document, error) {
	return nil, r.err
}

func TestSplitSource_OpenInterruptionIsRetryable(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{})
	// The context passed to SplitSource must stay live: a cancelled outer ctx
	// would fail earlier at the DB/blob reads (already correctly propagated,
	// unrelated to this guard). What we're isolating is Renderer.Open itself
	// reporting an interruption (e.g. its own internal pool context timing
	// out) while the caller's ctx is still fine.
	f.svc.Renderer = failOpenRenderer{err: context.Canceled}
	if err := f.svc.SplitSource(f.ctx, src.ID); err == nil {
		t.Fatal("interruption must be returned for retry, not swallowed as terminal")
	}
	got, gerr := f.st.Q.GetScanSource(f.ctx, src.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Error.Valid && got.Error.String != "" {
		t.Fatalf("interruption must not stamp a terminal error, got %q", got.Error.String)
	}
}
