package offline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HaoWen46/adagrade/internal/render"
)

// Page is one rasterized scan page, the unit everything downstream works on.
//
// Index is 1-based and GLOBAL: pages are numbered straight through the scan
// files in the order they were given on the command line, so page 7 means the
// same thing in the report, in pages/p0007.jpg and in the crops. SourcePDF and
// SourcePage keep the way back to the operator's own file, which is what they
// need to go look at a page the run could not place.
type Page struct {
	Index      int    // 1-based global index across all scan files, render order
	SourcePDF  string // as passed on the command line
	SourcePage int    // 1-based page number within SourcePDF
	JPEG       []byte
}

// PageFilename is the artifact name for a page's image: p0001.jpg, p0002.jpg,
// ... Fixed at four digits so a directory listing sorts in render order.
func PageFilename(index int) string { return fmt.Sprintf("p%04d.jpg", index) }

// RenderPages rasterizes every scan file to JPEG pages, writing each one to
// pagesDir/pNNNN.jpg and returning them in render order.
//
// One render.Document is open at a time. A PDFium document pins a worker from a
// pool of four (render.Document's contract), and the handle is instance-affine,
// so opening every scan up front would starve the pool on the fifth file and
// deadlock a run that was only trying to be tidy. Each file is opened, drained
// and closed before the next one is touched.
//
// Every failure is a *ScanError (exit 4) naming the file, because the operator's
// fix is always about a specific input: a path typo, a file that is not really a
// PDF, a scan that ends mid-stream. A run that produces zero pages in total is
// also a *ScanError — there is nothing to match, and saying so here beats an
// empty report later.
func RenderPages(ctx context.Context, r render.Renderer, scans []string, pagesDir string, dpi, longEdge, quality int) ([]Page, error) {
	if len(scans) == 0 {
		return nil, newScanError(nil, "no scan files to render: pass --scans FILE (repeatable) or list scan paths after all flags")
	}
	if err := os.MkdirAll(pagesDir, 0o755); err != nil {
		return nil, newScanError(err, "cannot create page directory %s", pagesDir)
	}

	opts := render.Options{DPI: dpi, MaxLongEdgePx: longEdge, JPEGQuality: quality}
	var pages []Page
	for _, scan := range scans {
		rendered, err := renderOne(ctx, r, scan, pagesDir, opts, len(pages))
		if err != nil {
			return nil, err
		}
		pages = append(pages, rendered...)
	}
	if len(pages) == 0 {
		return nil, newScanError(nil, "the scan files hold no pages: re-export them from the scanner")
	}
	return pages, nil
}

// renderOne renders every page of a single scan file. It exists so the
// document's Close is a defer with a scope that ENDS at the file — a deferred
// Close in RenderPages' loop body would hold every document until the whole run
// finished, which is the pool starvation the loop is written to avoid.
//
// startIndex is the number of pages already rendered, so the returned pages
// carry global indexes.
func renderOne(ctx context.Context, r render.Renderer, scan, pagesDir string, opts render.Options, startIndex int) ([]Page, error) {
	data, err := os.ReadFile(scan)
	if err != nil {
		return nil, newScanError(err, "cannot read scan file %s", scan)
	}
	doc, err := r.Open(ctx, data)
	if err != nil {
		return nil, newScanError(err, "scan file %s is not a readable PDF: re-export it, or pass the scan in a format the renderer accepts", scan)
	}
	defer doc.Close()

	count := doc.PageCount()
	pages := make([]Page, 0, count)
	for i := 0; i < count; i++ {
		rendered, err := doc.RenderPage(ctx, i, opts)
		if err != nil {
			return nil, newScanError(err, "cannot render page %d of scan file %s", i+1, scan)
		}
		page := Page{
			Index:      startIndex + i + 1,
			SourcePDF:  scan,
			SourcePage: i + 1,
			JPEG:       rendered.JPEG,
		}
		path := filepath.Join(pagesDir, PageFilename(page.Index))
		if err := os.WriteFile(path, page.JPEG, 0o644); err != nil {
			return nil, newScanError(err, "cannot write page image %s", path)
		}
		pages = append(pages, page)
	}
	return pages, nil
}
