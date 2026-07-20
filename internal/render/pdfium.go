package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

// PDFium renders through a pool of WASM PDFium workers. Instances are not
// goroutine-safe; the pool hands each call its own worker.
type PDFium struct {
	pool pdfium.Pool
}

// NewPDFium boots the WASM worker pool. Init is slow (~1s, instantiates the wasm
// module); call once at startup and share.
func NewPDFium() (*PDFium, error) {
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:  1,
		MaxIdle:  2,
		MaxTotal: 4,
	})
	if err != nil {
		return nil, fmt.Errorf("render: pdfium init: %w", err)
	}
	return &PDFium{pool: pool}, nil
}

func (p *PDFium) Close() error {
	return p.pool.Close()
}

const instanceTimeout = 30 * time.Second

// Open checks out ONE pool worker, loads the PDF once, and pins that worker to
// the returned Document until Close. The PDFium document handle is instance-affine
// (it belongs to the worker that opened it — see go-pdfium's Pdfium doc: "Documents
// and handles can't be shared between different instances"), so pinning the worker
// object is exactly the pinning the affinity requires; we hold the instance directly
// rather than round-tripping through the pool per call. This is the fix for the N+1
// full-document re-parse: PageCount + every RenderPage now reuse this single load.
func (p *PDFium) Open(ctx context.Context, pdf []byte) (Document, error) {
	inst, err := p.pool.GetInstance(instanceTimeout)
	if err != nil {
		return nil, fmt.Errorf("render: acquire worker: %w", err)
	}
	// The WASM OpenDocument memcpys the whole file into the instance before
	// parsing, so pdf is only referenced for the duration of this call — no copy
	// needed, matching the pre-F3 one-shot path.
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &pdf})
	if err != nil {
		// Airtight: release the worker on the error path so a failed Open never
		// leaks a pool slot (MaxTotal=4 — a leak starves all rendering).
		_ = inst.Close()
		return nil, fmt.Errorf("render: open pdf: %w", err)
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
		_ = inst.Close()
		return nil, fmt.Errorf("render: page count: %w", err)
	}
	return &pdfiumDoc{inst: inst, doc: doc.Document, pageCount: count.PageCount}, nil
}

// pdfiumDoc pins one pool worker (inst) and one loaded document (doc). Not
// goroutine-safe: a single document is served by a single worker, so concurrent
// use of one Document would race the worker — callers use one Document per
// goroutine (distinct Documents run concurrently on distinct pool workers).
type pdfiumDoc struct {
	inst      pdfium.Pdfium
	doc       references.FPDF_DOCUMENT
	pageCount int
	closed    bool
}

func (d *pdfiumDoc) PageCount() int { return d.pageCount }

// Close closes the document and returns the worker to the pool. Idempotent:
// a second call is a no-op, so a defer-Close plus an explicit early Close (or a
// double defer) is safe and never double-frees the worker or the doc handle.
func (d *pdfiumDoc) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	// Close the document first, then hand the worker back. inst.Close would also
	// release any open document, but closing explicitly keeps WASM memory bounded
	// if a worker is reused for many documents in sequence.
	_, _ = d.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: d.doc})
	return d.inst.Close()
}

// rasterize renders one page to an *image.RGBA at the resolution the options
// imply, returning the render response (whose Cleanup the caller MUST invoke to
// free WASM memory) alongside the target pixel dimensions. Shared by RenderPage
// and RenderPageImage.
func (d *pdfiumDoc) rasterize(pageIndex int, opts Options) (*image.RGBA, func(), error) {
	pageRef := requests.Page{ByIndex: &requests.PageByIndex{Document: d.doc, Index: pageIndex}}

	// Page size in points (1pt = 1/72"): px = pt/72 * DPI.
	size, err := d.inst.GetPageSize(&requests.GetPageSize{Page: pageRef})
	if err != nil {
		return nil, nil, fmt.Errorf("render: page size (index %d): %w", pageIndex, err)
	}
	wPx := int(size.Width / 72.0 * float64(opts.DPI))
	hPx := int(size.Height / 72.0 * float64(opts.DPI))
	if long := max(wPx, hPx); long > opts.MaxLongEdgePx {
		scale := float64(opts.MaxLongEdgePx) / float64(long)
		wPx = int(float64(wPx) * scale)
		hPx = int(float64(hPx) * scale)
	}
	if wPx < 1 || hPx < 1 {
		return nil, nil, fmt.Errorf("render: degenerate page size %.1fx%.1fpt", size.Width, size.Height)
	}

	rendered, err := d.inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page:  pageRef,
		Width: wPx, Height: hPx,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("render: rasterize page %d: %w", pageIndex, err)
	}
	return rendered.Result.Image, rendered.Cleanup, nil
}

// encodePage encodes an already-rasterized image to a Page. The encode path is
// identical to the pre-F3 one, so Page.JPEG bytes and Page.SHA256 are byte-stable
// (mask fingerprints depend on the image SHA — this must not change).
func encodePage(img image.Image, quality int) (Page, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return Page{}, fmt.Errorf("render: jpeg encode: %w", err)
	}
	b := img.Bounds()
	return Page{
		JPEG:   buf.Bytes(),
		Width:  b.Dx(),
		Height: b.Dy(),
		SHA256: sha256hex(buf.Bytes()),
	}, nil
}

// RenderPage rasterizes one page. The long-edge cap is applied by reducing the
// effective resolution before rendering (no post-hoc resampling), keeping handwriting
// crisp at the largest size the cap allows.
func (d *pdfiumDoc) RenderPage(ctx context.Context, pageIndex int, opts Options) (Page, error) {
	opts = opts.withDefaults()
	img, cleanup, err := d.rasterize(pageIndex, opts)
	if err != nil {
		return Page{}, err
	}
	defer cleanup() // WASM memory must be released after encode
	return encodePage(img, opts.JPEGQuality)
}

// RenderPageImage returns both the encoded Page and the decoded raster. Because
// the WASM render buffer is freed by cleanup(), the raster is copied into a
// caller-owned *image.RGBA that outlives this call (F8: scan crops the ID box
// from this raster instead of re-decoding the JPEG it just produced).
func (d *pdfiumDoc) RenderPageImage(ctx context.Context, pageIndex int, opts Options) (image.Image, Page, error) {
	opts = opts.withDefaults()
	img, cleanup, err := d.rasterize(pageIndex, opts)
	if err != nil {
		return nil, Page{}, err
	}
	defer cleanup()
	page, err := encodePage(img, opts.JPEGQuality)
	if err != nil {
		return nil, Page{}, err
	}
	// Deep-copy the raster so it survives cleanup() (which frees the WASM-backed
	// image). draw.Draw over a fresh RGBA of the same bounds copies every pixel.
	owned := image.NewRGBA(img.Bounds())
	draw.Draw(owned, owned.Bounds(), img, img.Bounds().Min, draw.Src)
	return owned, page, nil
}

// PageCount opens the PDF, reads the page count, and closes — a thin wrapper over
// Open for one-shot callers. Prefer Open when you also render pages.
func (p *PDFium) PageCount(ctx context.Context, pdf []byte) (int, error) {
	doc, err := p.Open(ctx, pdf)
	if err != nil {
		return 0, err
	}
	defer func() { _ = doc.Close() }()
	return doc.PageCount(), nil
}

// RenderPage opens the PDF, renders one page, and closes — a thin wrapper over
// Open for one-shot callers. Prefer Open when you render more than one page.
func (p *PDFium) RenderPage(ctx context.Context, pdf []byte, pageIndex int, opts Options) (Page, error) {
	doc, err := p.Open(ctx, pdf)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = doc.Close() }()
	return doc.RenderPage(ctx, pageIndex, opts)
}
