package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
)

// Fake is a deterministic Renderer for tests and offline development: every "PDF"
// has a fixed page count and each page renders as a flat-colored image whose color
// derives from the page index.
type Fake struct {
	Pages int
}

func NewFake(pages int) *Fake { return &Fake{Pages: pages} }

// Open returns an in-memory Document with the fake's fixed page count. No pool,
// no affinity — Close is a no-op — but it mirrors the real API so callers can be
// written once against Open/RenderPage/Close.
func (f *Fake) Open(ctx context.Context, pdf []byte) (Document, error) {
	return &fakeDoc{pages: f.Pages}, nil
}

func (f *Fake) PageCount(ctx context.Context, pdf []byte) (int, error) {
	return f.Pages, nil
}

func (f *Fake) RenderPage(ctx context.Context, pdf []byte, pageIndex int, opts Options) (Page, error) {
	_, page, err := renderFakePage(pageIndex, f.Pages, opts)
	return page, err
}

func (f *Fake) Close() error { return nil }

// fakeDoc is the trivial in-memory Document backing Fake.Open.
type fakeDoc struct{ pages int }

func (d *fakeDoc) PageCount() int { return d.pages }

func (d *fakeDoc) RenderPage(ctx context.Context, pageIndex int, opts Options) (Page, error) {
	_, page, err := renderFakePage(pageIndex, d.pages, opts)
	return page, err
}

func (d *fakeDoc) RenderPageImage(ctx context.Context, pageIndex int, opts Options) (image.Image, Page, error) {
	return renderFakePage(pageIndex, d.pages, opts)
}

// ProbeTextLoss implements TextLossProber: fake pages carry no text layer, so
// the report is always clean — callers wired against ProbeTextLoss work
// unchanged under the Fake.
func (d *fakeDoc) ProbeTextLoss(ctx context.Context, pageIndex int, raster image.Image) (TextLossReport, error) {
	if pageIndex < 0 || pageIndex >= d.pages {
		return TextLossReport{}, fmt.Errorf("render(fake): page %d out of range [0,%d)", pageIndex, d.pages)
	}
	return TextLossReport{}, nil
}

func (d *fakeDoc) Close() error { return nil }

// renderFakePage is the shared deterministic raster+encode used by every Fake
// entry point, so RenderPage and RenderPageImage return byte-identical Pages.
func renderFakePage(pageIndex, pages int, opts Options) (image.Image, Page, error) {
	if pageIndex < 0 || pageIndex >= pages {
		return nil, Page{}, fmt.Errorf("render(fake): page %d out of range [0,%d)", pageIndex, pages)
	}
	opts = opts.withDefaults()
	w, h := 200, 260
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	shade := uint8(40 + pageIndex*37)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255 - shade, G: 255 - shade/2, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: opts.JPEGQuality}); err != nil {
		return nil, Page{}, err
	}
	return img, Page{JPEG: buf.Bytes(), Width: w, Height: h, SHA256: sha256hex(buf.Bytes())}, nil
}
