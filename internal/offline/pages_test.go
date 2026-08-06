package offline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
)

// writeScan drops a placeholder scan file on disk. The Fake renderer never
// looks at the bytes, so the content only has to be non-empty.
func writeScan(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("%PDF-1.4 fake\n"), 0o644); err != nil {
		t.Fatalf("write scan: %v", err)
	}
	return path
}

// TestRenderPages_OrdersAndNamesPages pins the artifact contract: pages are
// numbered 1..N across ALL scan files in the order they were given, each page
// remembers where it came from, and every page lands on disk as pNNNN.jpg with
// the bytes the Page carries.
func TestRenderPages_OrdersAndNamesPages(t *testing.T) {
	dir := t.TempDir()
	a := writeScan(t, dir, "a.pdf")
	b := writeScan(t, dir, "b.pdf")
	pagesDir := filepath.Join(dir, "pages")

	pages, err := RenderPages(context.Background(), render.NewFake(2), []string{a, b}, pagesDir, 200, 1600, 80)
	if err != nil {
		t.Fatalf("RenderPages: %v", err)
	}
	if len(pages) != 4 {
		t.Fatalf("got %d pages, want 4", len(pages))
	}

	want := []struct {
		index      int
		source     string
		sourcePage int
	}{
		{1, a, 1}, {2, a, 2}, {3, b, 1}, {4, b, 2},
	}
	for i, w := range want {
		got := pages[i]
		if got.Index != w.index || got.SourcePDF != w.source || got.SourcePage != w.sourcePage {
			t.Errorf("pages[%d] = {Index:%d SourcePDF:%s SourcePage:%d}, want {%d %s %d}",
				i, got.Index, got.SourcePDF, got.SourcePage, w.index, w.source, w.sourcePage)
		}
		if len(got.JPEG) == 0 {
			t.Errorf("pages[%d] carries no JPEG bytes", i)
		}
	}

	// Files: exactly one per page, %04d-named, byte-identical to the Page.
	entries, err := os.ReadDir(pagesDir)
	if err != nil {
		t.Fatalf("read pages dir: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("pages dir holds %d entries, want 4", len(entries))
	}
	for i, p := range pages {
		path := filepath.Join(pagesDir, fmt.Sprintf("p%04d.jpg", i+1))
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(onDisk, p.JPEG) {
			t.Errorf("%s: on-disk bytes differ from Page.JPEG", path)
		}
		if _, err := jpeg.Decode(bytes.NewReader(onDisk)); err != nil {
			t.Errorf("%s does not decode as JPEG: %v", path, err)
		}
	}
}

// TestRenderPages_PaddingIsFourDigits pins the %04d naming past the single
// digit range, so a 10-page scan sorts lexicographically the way it renders.
func TestRenderPages_PaddingIsFourDigits(t *testing.T) {
	dir := t.TempDir()
	a := writeScan(t, dir, "a.pdf")
	pagesDir := filepath.Join(dir, "pages")

	if _, err := RenderPages(context.Background(), render.NewFake(10), []string{a}, pagesDir, 200, 1600, 80); err != nil {
		t.Fatalf("RenderPages: %v", err)
	}
	for _, name := range []string{"p0001.jpg", "p0009.jpg", "p0010.jpg"} {
		if _, err := os.Stat(filepath.Join(pagesDir, name)); err != nil {
			t.Errorf("expected %s: %v", name, err)
		}
	}
}

// TestRenderPages_RenderOptionsReachTheRenderer proves the caller's DPI /
// long-edge / quality actually travel: a lower JPEG quality must produce
// different (smaller) bytes than a higher one.
func TestRenderPages_RenderOptionsReachTheRenderer(t *testing.T) {
	dir := t.TempDir()
	a := writeScan(t, dir, "a.pdf")

	lo, err := RenderPages(context.Background(), render.NewFake(1), []string{a}, filepath.Join(dir, "lo"), 200, 1600, 30)
	if err != nil {
		t.Fatalf("RenderPages(lo): %v", err)
	}
	hi, err := RenderPages(context.Background(), render.NewFake(1), []string{a}, filepath.Join(dir, "hi"), 200, 1600, 95)
	if err != nil {
		t.Fatalf("RenderPages(hi): %v", err)
	}
	if len(lo[0].JPEG) >= len(hi[0].JPEG) {
		t.Errorf("quality 30 produced %d bytes, quality 95 produced %d: options are not reaching the renderer",
			len(lo[0].JPEG), len(hi[0].JPEG))
	}
}

// errRenderer fails on Open (a file the renderer cannot read as a PDF), which
// is the common real failure: a JPEG or a corrupt scan passed to --scans.
type errRenderer struct{ openErr, renderErr error }

func (r *errRenderer) Open(ctx context.Context, pdf []byte) (render.Document, error) {
	if r.openErr != nil {
		return nil, r.openErr
	}
	return &errDoc{renderErr: r.renderErr}, nil
}

func (r *errRenderer) PageCount(ctx context.Context, pdf []byte) (int, error) { return 1, nil }

func (r *errRenderer) RenderPage(ctx context.Context, pdf []byte, i int, o render.Options) (render.Page, error) {
	return render.Page{}, r.renderErr
}

func (r *errRenderer) Close() error { return nil }

type errDoc struct {
	renderErr error
	closed    int
}

func (d *errDoc) PageCount() int { return 1 }

func (d *errDoc) RenderPage(ctx context.Context, i int, o render.Options) (render.Page, error) {
	return render.Page{}, d.renderErr
}

func (d *errDoc) RenderPageImage(ctx context.Context, i int, o render.Options) (image.Image, render.Page, error) {
	return nil, render.Page{}, d.renderErr
}

func (d *errDoc) Close() error { d.closed++; return nil }

func TestRenderPages_Errors(t *testing.T) {
	dir := t.TempDir()
	a := writeScan(t, dir, "a.pdf")
	boom := errors.New("boom")

	tests := []struct {
		name     string
		renderer render.Renderer
		scans    []string
		want     string // substring the message must name
	}{
		{"unopenable scan", &errRenderer{openErr: boom}, []string{a}, "a.pdf"},
		{"unrenderable page", &errRenderer{renderErr: boom}, []string{a}, "a.pdf"},
		{"no scans", render.NewFake(2), nil, "no scan files"},
		{"missing file", render.NewFake(2), []string{filepath.Join(dir, "nope.pdf")}, "nope.pdf"},
		{"zero pages", render.NewFake(0), []string{a}, "no pages"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderPages(context.Background(), tc.renderer, tc.scans, filepath.Join(dir, "out-"+tc.name), 200, 1600, 80)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			var se *ScanError
			if !errors.As(err, &se) {
				t.Fatalf("want *ScanError (exit %d), got %T: %v", ExitScan, err, err)
			}
			if ExitCode(err) != ExitScan {
				t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitScan)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q should name %q", err.Error(), tc.want)
			}
		})
	}
}

// TestRenderPages_ClosesEveryDocument pins the pool discipline: PDFium
// documents pin a worker from a MaxTotal=4 pool, so a run over five scans must
// not hold five handles at once — each document is closed before the next opens.
func TestRenderPages_ClosesEveryDocument(t *testing.T) {
	dir := t.TempDir()
	var scans []string
	for _, n := range []string{"a.pdf", "b.pdf", "c.pdf", "d.pdf", "e.pdf"} {
		scans = append(scans, writeScan(t, dir, n))
	}
	tr := &trackingRenderer{pages: 2}
	if _, err := RenderPages(context.Background(), tr, scans, filepath.Join(dir, "pages"), 200, 1600, 80); err != nil {
		t.Fatalf("RenderPages: %v", err)
	}
	if tr.opened != 5 {
		t.Errorf("opened %d documents, want 5 (one per scan)", tr.opened)
	}
	if tr.maxOpen != 1 {
		t.Errorf("held %d documents open at once, want 1 (close before opening the next)", tr.maxOpen)
	}
	if tr.closed != 5 {
		t.Errorf("closed %d documents, want 5", tr.closed)
	}
}

// trackingRenderer counts concurrent open documents so the test above can see a
// leaked handle.
type trackingRenderer struct {
	pages                   int
	opened, closed, maxOpen int
	open                    int
}

func (r *trackingRenderer) Open(ctx context.Context, pdf []byte) (render.Document, error) {
	r.opened++
	r.open++
	if r.open > r.maxOpen {
		r.maxOpen = r.open
	}
	return &trackingDoc{r: r, inner: &render.Fake{Pages: r.pages}, pages: r.pages}, nil
}

func (r *trackingRenderer) PageCount(ctx context.Context, pdf []byte) (int, error) {
	return r.pages, nil
}

func (r *trackingRenderer) RenderPage(ctx context.Context, pdf []byte, i int, o render.Options) (render.Page, error) {
	return r.inner().RenderPage(ctx, pdf, i, o)
}

func (r *trackingRenderer) inner() *render.Fake { return &render.Fake{Pages: r.pages} }

func (r *trackingRenderer) Close() error { return nil }

type trackingDoc struct {
	r      *trackingRenderer
	inner  *render.Fake
	pages  int
	closed bool
}

func (d *trackingDoc) PageCount() int { return d.pages }

func (d *trackingDoc) RenderPage(ctx context.Context, i int, o render.Options) (render.Page, error) {
	return d.inner.RenderPage(ctx, nil, i, o)
}

func (d *trackingDoc) RenderPageImage(ctx context.Context, i int, o render.Options) (image.Image, render.Page, error) {
	p, err := d.inner.RenderPage(ctx, nil, i, o)
	return nil, p, err
}

func (d *trackingDoc) Close() error {
	if !d.closed {
		d.closed = true
		d.r.closed++
		d.r.open--
	}
	return nil
}
