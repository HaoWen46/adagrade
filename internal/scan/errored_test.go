// Batch-level bulk recovery for errored pages (2026-07-11): RetryErroredPages
// (optionally switching the batch's OCR provider — the escape from a batch
// whose provider terminal-errored every page) and DiscardErroredPages.
package scan

import (
	"errors"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// erroredBatch stages one 3-page batch (fake renderer default), renders every
// page's crops, and stamps a terminal error on the first two pages. Returns
// the batch id and the three page rows (pages[0], pages[1] errored; pages[2]
// healthy).
func erroredBatch(f fx, t *testing.T) (int64, []db.ScanPage) {
	t.Helper()
	src := looseSource(f, t, "run.pdf", "%PDF-1 "+t.Name(), NewBatch{OCREnabled: true, OCRProvider: "ghost", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[len(*f.renders)-1]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages: %d, %v", len(pages), err)
	}
	for _, p := range pages[:2] {
		if err := f.svc.setPageError(f.ctx, p.ID, "OCR provider unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	return src.BatchID, pages
}

func TestRetryErroredPages_ClearsAndReenqueuesIdentify(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	batchID, pages := erroredBatch(f, t)
	*f.identifies = (*f.identifies)[:0]
	*f.renders = (*f.renders)[:0]

	n, err := f.svc.RetryErroredPages(f.ctx, batchID, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retried = %d, want 2", n)
	}
	for _, p := range pages[:2] {
		got, err := f.st.Q.GetScanPage(f.ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Error.Valid {
			t.Fatalf("error not cleared on page %d: %+v", p.ID, got)
		}
	}
	// Both errored pages re-enqueue identify (their crops exist); no render.
	if len(*f.identifies) != 2 {
		t.Fatalf("identify enqueues = %v, want the two errored pages", *f.identifies)
	}
	if len(*f.renders) != 0 {
		t.Fatalf("must not re-enqueue render when crops exist, got %+v", *f.renders)
	}
	// No provider given: the batch's OCR fields stay untouched.
	batch, err := f.st.Q.GetScanBatch(f.ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if batch.OcrProvider.String != "ghost" || batch.OcrModel.String != "m" {
		t.Fatalf("batch OCR fields must be untouched: %+v", batch)
	}
	// Second call: nothing left in state "error".
	n, err = f.svc.RetryErroredPages(f.ctx, batchID, "", "", 1)
	if err != nil || n != 0 {
		t.Fatalf("second retry = %d, %v; want 0, nil", n, err)
	}
}

func TestRetryErroredPages_SwitchesProvider(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	batchID, _ := erroredBatch(f, t)
	f.svc.Providers = llm.StaticSource{"p": &fake.ScriptedProvider{NameStr: "p"}}

	n, err := f.svc.RetryErroredPages(f.ctx, batchID, "p", "m2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retried = %d, want 2", n)
	}
	batch, err := f.st.Q.GetScanBatch(f.ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if batch.OcrProvider.String != "p" || batch.OcrModel.String != "m2" {
		t.Fatalf("batch OCR fields not switched: %+v", batch)
	}
}

func TestRetryErroredPages_UnknownProviderRejected(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	batchID, pages := erroredBatch(f, t)
	f.svc.Providers = llm.StaticSource{"p": &fake.ScriptedProvider{NameStr: "p"}}

	_, err := f.svc.RetryErroredPages(f.ctx, batchID, "nope", "", 1)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for an unknown provider, got %v", err)
	}
	// Nothing cleared, batch untouched.
	got, gerr := f.st.Q.GetScanPage(f.ctx, pages[0].ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !got.Error.Valid {
		t.Fatal("page error must not be cleared when the provider is rejected")
	}
	batch, berr := f.st.Q.GetScanBatch(f.ctx, batchID)
	if berr != nil {
		t.Fatal(berr)
	}
	if batch.OcrProvider.String != "ghost" {
		t.Fatalf("batch provider must be untouched: %+v", batch)
	}
}

func TestRetryErroredPages_MissingCropsReenqueueRender(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "norender.pdf", "%PDF-1 no render", NewBatch{OCREnabled: true, OCRProvider: "ghost", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) == 0 {
		t.Fatalf("pages: %d, %v", len(pages), err)
	}
	for _, p := range pages {
		if err := f.svc.setPageError(f.ctx, p.ID, "page render failed"); err != nil {
			t.Fatal(err)
		}
	}
	*f.identifies = (*f.identifies)[:0]
	*f.renders = (*f.renders)[:0]

	n, err := f.svc.RetryErroredPages(f.ctx, src.BatchID, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(pages) {
		t.Fatalf("retried = %d, want %d", n, len(pages))
	}
	if len(*f.identifies) != 0 {
		t.Fatalf("must not enqueue identify without crops, got %v", *f.identifies)
	}
	covered := map[int64]bool{}
	for _, chunk := range *f.renders {
		if chunk.SourceID != src.ID {
			t.Fatalf("render chunk for wrong source: %+v", chunk)
		}
		for _, id := range chunk.PageIDs {
			covered[id] = true
		}
	}
	for _, p := range pages {
		if !covered[p.ID] {
			t.Fatalf("page %d missing from render chunks %+v", p.ID, *f.renders)
		}
	}
}

func TestDiscardErroredPages(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	batchID, pages := erroredBatch(f, t)

	n, err := f.svc.DiscardErroredPages(f.ctx, batchID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("discarded = %d, want 2", n)
	}
	for _, p := range pages[:2] {
		got, err := f.st.Q.GetScanPage(f.ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.DiscardedAt.Valid || got.Error.Valid {
			t.Fatalf("page %d must be discarded with its error cleared: %+v", p.ID, got)
		}
	}
	healthy, err := f.st.Q.GetScanPage(f.ctx, pages[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if healthy.DiscardedAt.Valid {
		t.Fatal("healthy page must not be discarded")
	}
	// Second call: nothing left in state "error".
	n, err = f.svc.DiscardErroredPages(f.ctx, batchID, 1)
	if err != nil || n != 0 {
		t.Fatalf("second discard = %d, %v; want 0, nil", n, err)
	}
}
