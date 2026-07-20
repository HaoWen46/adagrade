package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"testing"
)

// minimalPDF builds a valid n-page empty PDF (US Letter) with a correct xref table.
func minimalPDF(t *testing.T, pages int) []byte {
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
	kids := ""
	for i := 0; i < pages; i++ {
		if i > 0 {
			kids += " "
		}
		kids += fmt.Sprintf("%d 0 R", 3+i)
	}
	addObj("1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n")
	addObj(fmt.Sprintf("2 0 obj << /Type /Pages /Kids [%s] /Count %d >> endobj\n", kids, pages))
	for i := 0; i < pages; i++ {
		addObj(fmt.Sprintf("%d 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >> endobj\n", 3+i))
	}

	xrefAt := body.Len()
	write(fmt.Sprintf("xref\n0 %d\n", len(offsets)))
	write("0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		write(fmt.Sprintf("%010d 00000 n \n", off))
	}
	write(fmt.Sprintf("trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefAt))
	return body.Bytes()
}

func newTestRenderer(t *testing.T) *PDFium {
	t.Helper()
	r, err := NewPDFium()
	if err != nil {
		t.Fatalf("init pdfium: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestPDFium_PageCount(t *testing.T) {
	r := newTestRenderer(t)
	n, err := r.PageCount(context.Background(), minimalPDF(t, 3))
	if err != nil {
		t.Fatalf("PageCount: %v", err)
	}
	if n != 3 {
		t.Errorf("PageCount: got %d want 3", n)
	}
}

func TestPDFium_PageCount_RejectsGarbage(t *testing.T) {
	r := newTestRenderer(t)
	if _, err := r.PageCount(context.Background(), []byte("not a pdf")); err == nil {
		t.Error("garbage bytes should error")
	}
}

func TestPDFium_RenderPage_DPIAndDimensions(t *testing.T) {
	r := newTestRenderer(t)
	// US Letter is 612x792pt = 8.5x11in. At 100 DPI → 850x1100 px.
	page, err := r.RenderPage(context.Background(), minimalPDF(t, 1), 0, Options{DPI: 100, MaxLongEdgePx: 10000, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("RenderPage: %v", err)
	}
	if page.Width != 850 || page.Height != 1100 {
		t.Errorf("dimensions: got %dx%d want 850x1100", page.Width, page.Height)
	}
	img, err := jpeg.Decode(bytes.NewReader(page.JPEG))
	if err != nil {
		t.Fatalf("output is not a decodable JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != page.Width || b.Dy() != page.Height {
		t.Errorf("JPEG dims %dx%d != reported %dx%d", b.Dx(), b.Dy(), page.Width, page.Height)
	}
	if page.SHA256 == "" {
		t.Error("missing SHA256")
	}
}

func TestPDFium_RenderPage_LongEdgeCapReducesResolution(t *testing.T) {
	r := newTestRenderer(t)
	// At 250 DPI the long edge would be 2750px; cap 1100 → scaled to fit.
	page, err := r.RenderPage(context.Background(), minimalPDF(t, 1), 0, Options{DPI: 250, MaxLongEdgePx: 1100, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("RenderPage: %v", err)
	}
	if page.Height > 1100 || page.Width > 1100 {
		t.Errorf("long edge cap violated: %dx%d", page.Width, page.Height)
	}
	if page.Height < 1050 {
		t.Errorf("cap should downscale, not crush: %dx%d", page.Width, page.Height)
	}
}

func TestPDFium_RenderPage_IndexOutOfRange(t *testing.T) {
	r := newTestRenderer(t)
	if _, err := r.RenderPage(context.Background(), minimalPDF(t, 1), 5, Options{}); err == nil {
		t.Error("out-of-range page index should error")
	}
}

func TestFake_RendersDeterministicPages(t *testing.T) {
	f := NewFake(2)
	n, err := f.PageCount(context.Background(), []byte("whatever"))
	if err != nil || n != 2 {
		t.Fatalf("fake PageCount: %d %v", n, err)
	}
	p1, err := f.RenderPage(context.Background(), []byte("whatever"), 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := f.RenderPage(context.Background(), []byte("whatever"), 0, Options{})
	if p1.SHA256 != p2.SHA256 {
		t.Error("fake pages should be deterministic")
	}
	if _, err := f.RenderPage(context.Background(), nil, 2, Options{}); err == nil {
		t.Error("fake should reject out-of-range index")
	}
}

// ---- F3: document-scoped API ----

// TestPDFium_OpenMatchesWrapperBytes asserts the document-scoped path
// (Open → PageCount + RenderPage×N → Close) produces byte-identical Pages
// (JPEG bytes and SHA) to the one-shot wrapper path, so migrating callers to
// Open does not change any stored image or its mask fingerprint.
func TestPDFium_OpenMatchesWrapperBytes(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	pdf := minimalPDF(t, 3)
	opts := Options{DPI: 120, MaxLongEdgePx: 5000, JPEGQuality: 85}

	doc, err := r.Open(ctx, pdf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if doc.PageCount() != 3 {
		t.Fatalf("doc.PageCount() = %d, want 3", doc.PageCount())
	}
	for i := 0; i < 3; i++ {
		viaDoc, err := doc.RenderPage(ctx, i, opts)
		if err != nil {
			t.Fatalf("doc.RenderPage(%d): %v", i, err)
		}
		viaWrapper, err := r.RenderPage(ctx, pdf, i, opts)
		if err != nil {
			t.Fatalf("wrapper RenderPage(%d): %v", i, err)
		}
		if !bytes.Equal(viaDoc.JPEG, viaWrapper.JPEG) {
			t.Errorf("page %d: doc JPEG bytes differ from wrapper", i)
		}
		if viaDoc.SHA256 != viaWrapper.SHA256 {
			t.Errorf("page %d: doc SHA %s != wrapper SHA %s", i, viaDoc.SHA256, viaWrapper.SHA256)
		}
	}
	if err := doc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestPDFium_RenderPageImageMatchesRenderPage asserts RenderPageImage returns a
// Page byte-identical to RenderPage (same encode path — byte-stability that mask
// fingerprints rely on) AND a decoded raster whose dimensions match the Page, so
// the F8 crop-from-raster path sees exactly the pixels the JPEG was encoded from.
func TestPDFium_RenderPageImageMatchesRenderPage(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	opts := Options{DPI: 100, MaxLongEdgePx: 5000, JPEGQuality: 85}

	doc, err := r.Open(ctx, minimalPDF(t, 1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = doc.Close() }()

	viaPage, err := doc.RenderPage(ctx, 0, opts)
	if err != nil {
		t.Fatalf("RenderPage: %v", err)
	}
	img, viaImage, err := doc.RenderPageImage(ctx, 0, opts)
	if err != nil {
		t.Fatalf("RenderPageImage: %v", err)
	}
	if !bytes.Equal(viaPage.JPEG, viaImage.JPEG) || viaPage.SHA256 != viaImage.SHA256 {
		t.Error("RenderPageImage Page bytes/SHA differ from RenderPage")
	}
	if img == nil {
		t.Fatal("RenderPageImage returned nil raster")
	}
	b := img.Bounds()
	if b.Dx() != viaImage.Width || b.Dy() != viaImage.Height {
		t.Errorf("raster %dx%d != Page %dx%d", b.Dx(), b.Dy(), viaImage.Width, viaImage.Height)
	}
	// The raster must survive internal WASM cleanup: probe a pixel (any read that
	// would panic/garble on a freed buffer). A blank page is white.
	_, _, _, a := img.At(b.Min.X, b.Min.Y).RGBA()
	if a == 0 {
		t.Error("raster pixel has zero alpha; buffer may have been freed")
	}
}

// TestPDFium_CloseIdempotent asserts a second Close (and a Close after the whole
// doc has been used) is a harmless no-op — the discipline that lets callers both
// defer Close and Close early on error paths without double-freeing the worker.
func TestPDFium_CloseIdempotent(t *testing.T) {
	r := newTestRenderer(t)
	doc, err := r.Open(context.Background(), minimalPDF(t, 1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := doc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := doc.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

// TestPDFium_TwoConcurrentDocuments opens two Documents at once and renders on
// both, proving distinct Documents run on distinct pool workers (the pool has 4,
// so 2 concurrent is comfortably under MaxTotal).
func TestPDFium_TwoConcurrentDocuments(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	opts := Options{DPI: 100, MaxLongEdgePx: 5000, JPEGQuality: 85}

	d1, err := r.Open(ctx, minimalPDF(t, 2))
	if err != nil {
		t.Fatalf("Open d1: %v", err)
	}
	defer func() { _ = d1.Close() }()
	d2, err := r.Open(ctx, minimalPDF(t, 2))
	if err != nil {
		t.Fatalf("Open d2: %v", err)
	}
	defer func() { _ = d2.Close() }()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for k, d := range []Document{d1, d2} {
		wg.Add(1)
		go func(k int, d Document) {
			defer wg.Done()
			if _, e := d.RenderPage(ctx, 0, opts); e != nil {
				errs[k] = e
			}
		}(k, d)
	}
	wg.Wait()
	for k, e := range errs {
		if e != nil {
			t.Errorf("concurrent doc %d render: %v", k, e)
		}
	}
}

// TestPDFium_NoWorkerLeakOnClose is the handle-leak guard: it opens far more
// documents than the pool size (MaxTotal=4), sequentially, each Closed before
// the next Open. If Close failed to return the worker, the (blank) render on the
// 5th Open would block on GetInstance until the 30s instanceTimeout and fail —
// so this test completing quickly is the assertion that Close reliably frees the
// worker. (A deliberate no-Close leak test would deadlock the pool, so the pool
// exposes no live checkout count to assert on directly; sequential reuse past the
// pool size past MaxTotal is the observable proxy.)
func TestPDFium_NoWorkerLeakOnClose(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()
	opts := Options{DPI: 72, MaxLongEdgePx: 2000, JPEGQuality: 80}
	const n = 12 // 3× MaxTotal
	for i := 0; i < n; i++ {
		doc, err := r.Open(ctx, minimalPDF(t, 1))
		if err != nil {
			t.Fatalf("Open iter %d (worker likely leaked from a prior iter): %v", i, err)
		}
		if _, err := doc.RenderPage(ctx, 0, opts); err != nil {
			_ = doc.Close()
			t.Fatalf("RenderPage iter %d: %v", i, err)
		}
		if err := doc.Close(); err != nil {
			t.Fatalf("Close iter %d: %v", i, err)
		}
	}
}

// TestFake_DocumentRenderPageImage pins the fake's document path used by scan
// tests: RenderPageImage returns a non-nil raster and a Page identical to the
// fake's RenderPage, and out-of-range indices error.
func TestFake_DocumentRenderPageImage(t *testing.T) {
	f := NewFake(2)
	doc, err := f.Open(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("fake Open: %v", err)
	}
	defer func() { _ = doc.Close() }()
	if doc.PageCount() != 2 {
		t.Fatalf("fake doc PageCount = %d, want 2", doc.PageCount())
	}
	img, page, err := doc.RenderPageImage(context.Background(), 0, Options{})
	if err != nil {
		t.Fatalf("fake RenderPageImage: %v", err)
	}
	var _ image.Image = img
	if img == nil {
		t.Fatal("fake RenderPageImage returned nil raster")
	}
	viaPage, _ := doc.RenderPage(context.Background(), 0, Options{})
	if !bytes.Equal(page.JPEG, viaPage.JPEG) || page.SHA256 != viaPage.SHA256 {
		t.Error("fake RenderPageImage Page differs from RenderPage")
	}
	if _, _, err := doc.RenderPageImage(context.Background(), 5, Options{}); err == nil {
		t.Error("fake RenderPageImage should reject out-of-range index")
	}
}
