package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// SplitSource counts the source's pages and materializes one scan_pages row per
// page. Idempotent: CreateScanPage no-ops on (source_id, page_index) conflicts,
// and rows are re-listed before the render fan-out so redelivery re-enqueues
// every page (render itself is idempotent).
func (s *Service) SplitSource(ctx context.Context, sourceID int64) error {
	src, err := s.Store.Q.GetScanSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: split: load source: %w", err)
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, src.BatchID)
	if err != nil {
		return fmt.Errorf("scan: split: load batch: %w", err)
	}

	pageCount := 1
	if src.SourceKind == "pdf" {
		data, err := s.readAllSource(ctx, src.SourceRef)
		if err != nil {
			return fmt.Errorf("scan: split: read source: %w", err)
		}
		doc, err := s.Renderer.Open(ctx, data)
		if err != nil {
			if isInterruption(ctx, err) {
				return err
			}
			// Deterministic: a corrupt PDF never gets better on retry.
			_ = s.Store.Q.SetScanSourceError(ctx, db.SetScanSourceErrorParams{
				ID: sourceID, Error: textOrNull("source is not a readable PDF"),
			})
			return nil
		}
		pageCount = doc.PageCount()
		_ = doc.Close()
		if pageCount == 0 {
			_ = s.Store.Q.SetScanSourceError(ctx, db.SetScanSourceErrorParams{
				ID: sourceID, Error: textOrNull("source PDF has no pages"),
			})
			return nil
		}
	}

	for i := 0; i < pageCount; i++ {
		_, err := s.Store.Q.CreateScanPage(ctx, db.CreateScanPageParams{
			SourceID: sourceID, BatchID: src.BatchID,
			AssessmentID: batch.AssessmentID, PageIndex: int32(i),
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("scan: split: create page: %w", err)
		}
	}
	if err := s.Store.Q.SetScanSourcePageCount(ctx, db.SetScanSourcePageCountParams{
		ID: sourceID, PageCount: int4Of(pageCount),
	}); err != nil {
		return fmt.Errorf("scan: split: record page count: %w", err)
	}

	pages, err := s.Store.Q.ListScanPagesForSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: split: list pages: %w", err)
	}
	if s.EnqueueRenderPages == nil {
		return nil
	}
	return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		for start := 0; start < len(pages); start += renderChunkSize {
			end := min(start+renderChunkSize, len(pages))
			ids := make([]int64, 0, end-start)
			for _, p := range pages[start:end] {
				// Skip only pages with nothing left to do here: rendered pages
				// that are still unidentified must be re-chunked so RenderPages
				// re-enqueues their identify jobs (redelivery stranding).
				if p.ImageRef.Valid && (p.IdentifiedAt.Valid || p.AssignedStudentID.Valid || p.DiscardedAt.Valid) {
					continue
				}
				ids = append(ids, p.ID)
			}
			if len(ids) == 0 {
				continue
			}
			if err := s.EnqueueRenderPages(ctx, tx, sourceID, ids); err != nil {
				return err
			}
		}
		return nil
	})
}

// readAllSource reads a source blob bounded by MaxSourceBytes (PDF sources may
// far exceed the per-entry cap readAll enforces).
func (s *Service) readAllSource(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxSourceBytes+1))
}

// RenderPages renders one chunk of a source's pages: page JPG + the three region
// crops per page, then enqueues identify for the chunk.
func (s *Service) RenderPages(ctx context.Context, sourceID int64, pageIDs []int64) error {
	src, err := s.Store.Q.GetScanSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: render: load source: %w", err)
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, src.BatchID)
	if err != nil {
		return fmt.Errorf("scan: render: load batch: %w", err)
	}
	regions, err := s.Store.Q.ListIDRegions(ctx, batch.AssessmentID)
	if err != nil {
		return fmt.Errorf("scan: render: load regions: %w", err)
	}
	byKind := regionByKind(regions)
	if len(byKind) != requiredRegionKinds {
		// Regions were deleted between upload and render; deterministic.
		for _, id := range pageIDs {
			s.setPageError(ctx, id, "id regions incomplete; redraw and retry")
		}
		return nil
	}

	data, err := s.readAllSource(ctx, src.SourceRef)
	if err != nil {
		return fmt.Errorf("scan: render: read source: %w", err)
	}

	var doc render.Document
	if src.SourceKind == "pdf" {
		doc, err = s.Renderer.Open(ctx, data)
		if err != nil {
			if isInterruption(ctx, err) {
				return err
			}
			for _, id := range pageIDs {
				s.setPageError(ctx, id, "source is not a readable PDF")
			}
			return nil
		}
		defer doc.Close()
	}

	var identify []int64
	for _, pageID := range pageIDs {
		page, err := s.Store.Q.GetScanPage(ctx, pageID)
		if err != nil {
			return fmt.Errorf("scan: render: load page: %w", err)
		}
		if page.ImageRef.Valid {
			// Idempotent redelivery: already rendered, but if the first
			// attempt died before the end-of-chunk identify enqueue, the
			// page would be stranded rendered-but-unidentified — re-enqueue
			// unless identification already happened or is moot.
			if !page.IdentifiedAt.Valid && !page.AssignedStudentID.Valid && !page.DiscardedAt.Valid {
				identify = append(identify, page.ID)
			}
			continue
		}

		var raster image.Image
		var pg render.Page
		if src.SourceKind == "pdf" {
			raster, pg, err = doc.RenderPageImage(ctx, int(page.PageIndex), s.Opts)
		} else {
			raster, pg, err = ingest.NormalizeImageRaster(data, s.Opts)
		}
		if err != nil {
			if isInterruption(ctx, err) {
				return err
			}
			s.setPageError(ctx, pageID, "page render failed")
			continue
		}

		// Non-embedded CID/CJK glyphs rasterize as nothing while the text layer
		// survives — flag it so the text_render_loss warning can surface pages
		// the AI would grade with content missing. Image sources zero-report.
		textLoss := 0
		if src.SourceKind == "pdf" {
			if rep, perr := render.ProbeTextLoss(ctx, doc, int(page.PageIndex), raster); perr == nil {
				textLoss = rep.SuspectRuns
			}
		}

		pageKey := fmt.Sprintf("assessments/%d/scans/%d/pages/%d-%s.jpg",
			batch.AssessmentID, batch.ID, pageID, pg.SHA256[:8])
		if _, _, err := s.Blobs.Put(ctx, pageKey, bytes.NewReader(pg.JPEG)); err != nil {
			return fmt.Errorf("scan: render: store page image: %w", err)
		}

		cropRefs := map[string]string{}
		cropFailed := false
		for _, kind := range []string{"student_id", "name", "problem_id"} {
			crop, err := imaging.CropImage(raster, []imaging.Region{byKind[kind]}, cropQuality)
			if err != nil {
				s.setPageError(ctx, pageID, "region crop failed; check the drawn regions")
				cropFailed = true
				break
			}
			key := fmt.Sprintf("assessments/%d/scans/%d/idcrop/%d-%s-%s.jpg",
				batch.AssessmentID, batch.ID, pageID, kind, crop.SHA256()[:8])
			if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(crop.JPEG())); err != nil {
				return fmt.Errorf("scan: render: store crop: %w", err)
			}
			cropRefs[kind] = key
		}
		if cropFailed {
			continue
		}

		if err := s.Store.Q.SetScanPageRendered(ctx, db.SetScanPageRenderedParams{
			ID: pageID, ImageRef: textOrNull(pageKey), ImageSha256: textOrNull(pg.SHA256),
			ImageWidth: int4Of(pg.Width), ImageHeight: int4Of(pg.Height),
			StudentIDCropRef: textOrNull(cropRefs["student_id"]),
			NameCropRef:      textOrNull(cropRefs["name"]),
			ProblemCropRef:   textOrNull(cropRefs["problem_id"]),
			TextLossRuns:     int32(textLoss),
		}); err != nil {
			return fmt.Errorf("scan: render: record: %w", err)
		}
		identify = append(identify, pageID)
	}

	if len(identify) == 0 {
		return nil
	}
	if (batch.OcrEnabled || s.Local != nil) && s.EnqueueIdentifyPages != nil {
		return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueIdentifyPages(ctx, tx, identify)
		})
	}
	// No OCR rung will ever run for this batch: identification is vacuously
	// complete, and the page must surface as an orphan for manual assignment
	// instead of sitting in "processing" forever.
	for _, pageID := range identify {
		if err := s.Store.Q.SetScanPageIdentified(ctx, db.SetScanPageIdentifiedParams{
			ID:                  pageID,
			OcrStudentID:        textOrNull(""),
			OcrName:             textOrNull(""),
			OcrProblem:          textOrNull(""),
			OcrStudentIDLegible: boolOf(false),
			OcrNameLegible:      boolOf(false),
			OcrProblemLegible:   boolOf(false),
			ProposedStudentID:   int8OrNull(0),
			ProposedProblemID:   int8OrNull(0),
		}); err != nil {
			return fmt.Errorf("scan: render: mark identified (no ocr): %w", err)
		}
	}
	return nil
}

// regionByKind converts the typed id_regions rows to imaging.Regions keyed by kind.
func regionByKind(regions []db.IDRegion) map[string]imaging.Region {
	m := make(map[string]imaging.Region, len(regions))
	for _, r := range regions {
		m[r.Kind] = imaging.Region{
			X: float64(r.X), Y: float64(r.Y), W: float64(r.W), H: float64(r.H),
			Color: r.Color, Padding: float64(r.Padding),
		}
	}
	return m
}
