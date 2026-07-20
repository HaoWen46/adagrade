// Package render is the Renderer seam (spec §2/§7): PDF pages → JPEG images.
// Primary implementation is PDFium compiled to WASM (wazero) — permissively licensed,
// no cgo, crash-isolated. The submission PDF is never split; pages are rendered
// straight from the source bytes (docs/DECISIONS.md D1).
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
)

// Options are the per-render knobs. Zero values take the defaults below.
type Options struct {
	DPI           int // rasterization density; 200-300 suits handwriting (spec §7)
	MaxLongEdgePx int // hard cap on the longer image edge — vision-model limit + cost lever
	JPEGQuality   int
}

// Defaults chosen per spec §7 (250 DPI ≈ legible handwriting; ~2200px long edge keeps
// tokens/cost bounded on vision models; JPEG q85 balances artifacts vs size).
const (
	DefaultDPI           = 250
	DefaultMaxLongEdgePx = 2200
	DefaultJPEGQuality   = 85
)

func (o Options) withDefaults() Options {
	if o.DPI <= 0 {
		o.DPI = DefaultDPI
	}
	if o.MaxLongEdgePx <= 0 {
		o.MaxLongEdgePx = DefaultMaxLongEdgePx
	}
	if o.JPEGQuality <= 0 {
		o.JPEGQuality = DefaultJPEGQuality
	}
	return o
}

// Page is one rasterized page.
type Page struct {
	JPEG   []byte
	Width  int
	Height int
	SHA256 string // hex sha of the JPEG bytes (image provenance, D1)
}

// Document is a single PDF opened ONCE against one pooled worker (F3). The
// underlying PDFium document handle is instance-affine — a document belongs to
// the worker instance that opened it and cannot be used from another — so a
// Document pins its worker for its whole lifetime. All PageCount/RenderPage
// calls on one Document reuse that single load, killing the N+1 full-document
// re-parse (each PDFium OpenDocument mallocs+memcpys the whole file into the
// WASM instance before parsing).
//
// Discipline (no finalizers): a Document holds a scarce pool worker (MaxTotal=4),
// so a leaked handle starves the pool. Callers MUST Close every Document,
// always via defer immediately after a successful Open, and must NOT rely on a
// finalizer to reclaim it. Close returns the worker to the pool and is
// idempotent (safe under double-Close).
type Document interface {
	PageCount() int
	RenderPage(ctx context.Context, pageIndex int, opts Options) (Page, error)
	// RenderPageImage rasterizes a page and returns BOTH the encoded Page
	// (byte-identical to RenderPage — same encode path, so image SHAs and mask
	// fingerprints are unchanged) and the decoded raster it came from, so a
	// caller that also needs to crop (scan's ID box, F8) does not have to
	// jpeg-Decode the JPEG it just encoded. The returned image is caller-owned
	// (it outlives any internal WASM cleanup).
	RenderPageImage(ctx context.Context, pageIndex int, opts Options) (image.Image, Page, error)
	// Close releases the document and returns the pooled worker. Idempotent.
	Close() error
}

// Renderer is the seam ingestion depends on. Open is the efficient primary API
// (one document load per file); PageCount/RenderPage are kept as thin
// Open→op→Close wrappers so existing one-shot callers keep compiling.
type Renderer interface {
	Open(ctx context.Context, pdf []byte) (Document, error)
	PageCount(ctx context.Context, pdf []byte) (int, error)
	RenderPage(ctx context.Context, pdf []byte, pageIndex int, opts Options) (Page, error)
	Close() error
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
