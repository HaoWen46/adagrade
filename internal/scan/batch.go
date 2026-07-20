package scan

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// zipReader wraps openZip's (io.ReaderAt, size) pair in a *zip.Reader.
func zipReader(ra io.ReaderAt, size int64) (*zip.Reader, error) {
	return zip.NewReader(ra, size)
}

// NewBatch are the operator-chosen OCR options for one uploaded scanner run.
type NewBatch struct {
	OCREnabled  bool
	OCRProvider string
	OCRModel    string
}

// SourceUpload is one uploaded source file, streamed — CreateBatch never buffers
// a whole scanner PDF in memory.
type SourceUpload struct {
	Filename string
	R        io.Reader
}

// BatchView reports one CreateBatch call's outcome.
type BatchView struct {
	Batch   db.ScanBatch
	Created int
	Skipped []SkipInfo
}

// ErrRegionsIncomplete: identification is impossible without all three region
// kinds, so the batch is rejected at the door instead of half-running (spec §5).
var ErrRegionsIncomplete = errors.New("scan: draw the three ID regions (student ID, name, problem) before uploading scans")

// ErrOCRProviderRequired: cloud identification has no default provider — an
// OCR-enabled batch with no provider would send every page's identify to
// Provider(ctx, "") and terminal-error the whole batch, so it is rejected at
// the door like ErrRegionsIncomplete.
var ErrOCRProviderRequired = errors.New("scan: choose an OCR provider — cloud identification has no default")

const requiredRegionKinds = 3

func (s *Service) regionsComplete(ctx context.Context, assessmentID int64) error {
	regions, err := s.Store.Q.ListIDRegions(ctx, assessmentID)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, r := range regions {
		kinds[r.Kind] = true
	}
	if len(kinds) != requiredRegionKinds {
		return ErrRegionsIncomplete
	}
	return nil
}

// CreateBatch creates the batch row plus its sources. Loose sources enqueue one
// scan.split each; a zip stores the archive and enqueues one scan.expand.
func (s *Service) CreateBatch(ctx context.Context, assessmentID int64, nb NewBatch, sources []SourceUpload, zip io.Reader, actor int64) (BatchView, error) {
	if (len(sources) == 0) == (zip == nil) {
		return BatchView{}, errors.New("scan: provide loose sources or a zip, not both or neither")
	}
	if nb.OCREnabled && nb.OCRProvider == "" {
		return BatchView{}, ErrOCRProviderRequired
	}
	if err := s.regionsComplete(ctx, assessmentID); err != nil {
		return BatchView{}, err
	}
	batch, err := s.Store.Q.CreateScanBatch(ctx, db.CreateScanBatchParams{
		AssessmentID: assessmentID,
		OcrEnabled:   nb.OCREnabled,
		OcrProvider:  textOrNull(nb.OCRProvider),
		OcrModel:     textOrNull(nb.OCRModel),
		SourceRef:    textOrNull(""),
		CreatedBy:    int8OrNull(actor),
	})
	if err != nil {
		return BatchView{}, fmt.Errorf("scan: create batch: %w", err)
	}
	if zip != nil {
		return s.createZipBatch(ctx, batch, zip)
	}
	return s.createLooseBatch(ctx, batch, sources)
}

func (s *Service) createLooseBatch(ctx context.Context, batch db.ScanBatch, sources []SourceUpload) (BatchView, error) {
	view := BatchView{Batch: batch}
	var sourceIDs []int64
	for n, up := range sources {
		ext, ok := acceptedExt(up.Filename)
		if !ok {
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "unknown_extension"})
			continue
		}
		cap := MaxSourceBytes // scanner PDFs
		kind := "pdf"
		if ext != "pdf" {
			cap = int64(MaxEntryBytes) // loose page images
			kind = "image"
		}
		key := fmt.Sprintf("assessments/%d/scans/%d/src/%d.%s", batch.AssessmentID, batch.ID, n, ext)
		sha, size, err := s.Blobs.Put(ctx, key, io.LimitReader(up.R, cap+1))
		if err != nil {
			return view, fmt.Errorf("scan: store source: %w", err)
		}
		if size == 0 {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "empty"})
			continue
		}
		if size > cap {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "too_large"})
			continue
		}
		src, err := s.Store.Q.CreateScanSource(ctx, db.CreateScanSourceParams{
			BatchID: batch.ID, OriginalFilename: baseName(up.Filename),
			SourceRef: key, SourceSha256: sha, SourceKind: kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "duplicate"})
			continue
		}
		if err != nil {
			return view, fmt.Errorf("scan: record source: %w", err)
		}
		view.Created++
		sourceIDs = append(sourceIDs, src.ID)
	}
	if len(sourceIDs) > 0 && s.EnqueueSplit != nil {
		if err := s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueSplit(ctx, tx, sourceIDs)
		}); err != nil {
			return view, fmt.Errorf("scan: enqueue split: %w", err)
		}
	}
	return view, nil
}

func (s *Service) createZipBatch(ctx context.Context, batch db.ScanBatch, zip io.Reader) (BatchView, error) {
	view := BatchView{Batch: batch}
	key := fmt.Sprintf("assessments/%d/scans/%d/upload.zip", batch.AssessmentID, batch.ID)
	_, size, err := s.Blobs.Put(ctx, key, io.LimitReader(zip, MaxZipBytes+1))
	if err != nil {
		return view, fmt.Errorf("scan: store zip: %w", err)
	}
	if size > MaxZipBytes {
		_ = s.Blobs.Delete(ctx, key)
		return view, errors.New("scan: zip exceeds the archive size cap")
	}
	if err := s.Store.Q.SetBatchSourceRef(ctx, db.SetBatchSourceRefParams{ID: batch.ID, SourceRef: textOrNull(key)}); err != nil {
		return view, fmt.Errorf("scan: record zip ref: %w", err)
	}
	if s.EnqueueExpand != nil {
		if err := s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueExpand(ctx, tx, batch.ID)
		}); err != nil {
			return view, fmt.Errorf("scan: enqueue expand: %w", err)
		}
	}
	return view, nil
}

// Expand is the scan.expand worker body. Idempotent: re-delivery re-reads the
// zip; existing sources dedupe on (batch_id, source_sha256).
func (s *Service) Expand(ctx context.Context, batchID int64) error {
	batch, err := s.Store.Q.GetScanBatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("scan: expand: load batch: %w", err)
	}
	if !batch.SourceRef.Valid || batch.SourceRef.String == "" {
		return nil // loose batch; nothing to expand
	}
	ra, size, closeFn, err := s.openZip(ctx, batch.SourceRef.String)
	if err != nil {
		return fmt.Errorf("scan: expand: open zip: %w", err)
	}
	defer closeFn()
	zr, err := zipReader(ra, size)
	if err != nil {
		return fmt.Errorf("scan: expand: read zip: %w", err)
	}
	var sourceIDs []int64
	for _, entry := range zr.File {
		if !acceptZipEntry(entry) {
			continue
		}
		ext, ok := acceptedExt(entry.Name)
		if !ok {
			continue
		}
		data, err := readZipEntry(entry)
		if err != nil || len(data) == 0 || len(data) > MaxEntryBytes {
			continue
		}
		key, sha, kind, err := s.storeEntryBlob(ctx, batch.AssessmentID, batch.ID, data, ext)
		if err != nil {
			return fmt.Errorf("scan: expand: store entry: %w", err)
		}
		src, err := s.Store.Q.CreateScanSource(ctx, db.CreateScanSourceParams{
			BatchID: batch.ID, OriginalFilename: baseName(entry.Name),
			SourceRef: key, SourceSha256: sha, SourceKind: kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // duplicate under redelivery
		}
		if err != nil {
			return fmt.Errorf("scan: expand: record entry: %w", err)
		}
		sourceIDs = append(sourceIDs, src.ID)
	}
	if len(sourceIDs) > 0 && s.EnqueueSplit != nil {
		return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueSplit(ctx, tx, sourceIDs)
		})
	}
	return nil
}
