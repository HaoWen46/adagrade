// Package scan proposes a roster match for a scanned exam page from its OCR'd
// identity-box contents (student ID + name, names usually Chinese) and owns the
// page-level scan-intake pipeline (design spec 2026-07-04): batches of scanned
// exam papers land in scan_sources / scan_pages, are split into one row per
// physical page, rendered (per-kind identity crops), optionally OCR'd and
// roster-matched into a proposal, human-confirmed one page at a time, then
// promoted through the existing ingest tail at finalize.
//
// Privacy (D14/D19): OCR text lives only in scan_pages columns — never in slog,
// error strings, or job args. A provider request carries identity XOR answer
// content: identification only ever sends an imaging crop, and only when the
// assessment defines id_regions and the batch has ocr_enabled (no crop ⇒ no
// provider call, ever).
package scan

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/ocr"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

const (
	// MaxEntryBytes bounds one zip entry / loose image (matches ingest's cap).
	MaxEntryBytes = ingest.MaxPDFBytes // 50 MiB
	// MaxZipBytes bounds one uploaded zip archive.
	MaxZipBytes = 2 << 30 // 2 GiB
	cropQuality = 85
	// renderChunkSize pages share one scan.render_pages job (one PDFium
	// document open per chunk instead of per page).
	renderChunkSize = 25
)

// MaxSourceBytes bounds one uploaded scanner PDF. A var so tests can shrink it;
// runtime configurability is deferred (spec §5, YAGNI).
var MaxSourceBytes int64 = 2 << 30

// localOCRMinConfidence is the D24 default confidence gate for the local-OCR
// rung: lines below this are excluded from PickID/PickName entirely (treated
// as if the reader never produced them), not merely down-weighted.
const localOCRMinConfidence = 0.60

// engineLocal is the ocr_engine value the local rung writes; the cloud rung
// writes the resolved provider name instead.
const engineLocal = "local"

// Service wires the page-level staging pipeline's seams together. The Enqueue
// closures are injected by the queue package so row creation can enqueue the
// split/render/identify/promote jobs inside the same transaction as the row
// writes (spec §5, mirroring runner.EnqueueLeaves); all may be nil in tests, in
// which case the enqueue is skipped.
type Service struct {
	Store     *store.Store
	Blobs     blobstore.Store
	Renderer  render.Renderer
	Opts      render.Options
	Providers llm.ProviderSource
	Ingest    *ingest.Service
	Log       *slog.Logger

	// Local is the on-device OCR rung (D24): a nil value means the local
	// reader is not installed (no onnxruntime/model available), in which case
	// identification runs the cloud-VLM path exactly as before. When set, it
	// is tried FIRST — before any provider call — and its own confident,
	// roster-matching output finishes identification with zero network calls.
	Local ocr.Reader

	// EnqueueExpand unzips a batch's stored zip into scan_sources rows.
	EnqueueExpand func(ctx context.Context, tx pgx.Tx, batchID int64) error
	// EnqueueSplit splits one or more sources into per-page scan_pages rows.
	EnqueueSplit func(ctx context.Context, tx pgx.Tx, sourceIDs []int64) error
	// EnqueueRenderPages renders a chunk of a source's pages (identity crops).
	EnqueueRenderPages func(ctx context.Context, tx pgx.Tx, sourceID int64, pageIDs []int64) error
	// EnqueueIdentifyPages OCRs + roster-matches a set of pages.
	EnqueueIdentifyPages func(ctx context.Context, tx pgx.Tx, pageIDs []int64) error
	// EnqueuePromotePages enqueues one promote job per assigned unpromoted page
	// at finalize (D27, F1).
	EnqueuePromotePages func(ctx context.Context, tx pgx.Tx, items []PromotePage) error
}

// PromotePage is one page to promote off-request (D27, F1). Force carries the
// finalize call's force flag; Actor is the finalizing user, both threaded
// through to the promote job so it can call ingest with the right guards/audit
// actor.
type PromotePage struct {
	PageID int64
	Force  bool
	Actor  int64
}

// Upload is one loose file in a CreateBatch call.
type Upload struct {
	Filename string
	Data     []byte
}

// SkipInfo records one rejected/duplicate entry (filename only — never contents).
type SkipInfo struct {
	Filename string `json:"filename"`
	Reason   string `json:"reason"` // "unknown_extension" | "empty" | "too_large" | "duplicate"
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// readAll fetches a blob fully (bounded at the entry cap +1 for safety).
func (s *Service) readAll(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxEntryBytes+1))
}

// setPageError records a terminal, PII-free error on a page row.
func (s *Service) setPageError(ctx context.Context, pageID int64, msg string) error {
	return s.Store.Q.SetScanPageError(ctx, db.SetScanPageErrorParams{ID: pageID, Error: textOrNull(msg)})
}

// setPromotionError records a PII-free error from a promote job ONLY when the
// row is still unpromoted (D27 review, F2). Two rapid finalizes enqueue
// duplicate promote jobs for the same page; the winner promotes and links a
// submission, the loser's ingest then hits the active-unique index (23505) and
// reports a rejection. An unguarded write would stamp "promotion rejected: …"
// onto the row the winner just promoted. The WHERE submission_id IS NULL
// clause makes the losing write a harmless no-op.
func (s *Service) setPromotionError(ctx context.Context, pageID int64, msg string) error {
	return s.Store.Q.SetScanPagePromotionError(ctx, db.SetScanPagePromotionErrorParams{ID: pageID, Error: textOrNull(msg)})
}

// roster loads the non-withdrawn matching candidates.
func (s *Service) roster(ctx context.Context) ([]RosterEntry, error) {
	students, err := s.Store.Q.ListActiveStudents(ctx)
	if err != nil {
		return nil, err
	}
	roster := make([]RosterEntry, 0, len(students))
	for _, st := range students {
		roster = append(roster, RosterEntry{ID: st.ID, ExternalID: st.StudentID, Name: st.Name})
	}
	return roster, nil
}

// isInterruption reports whether a failed call was a shutdown/timeout
// interruption rather than a genuine verdict (F17). True when the returned
// error is (or wraps) context.Canceled/DeadlineExceeded, or the context itself
// is already done (catching a provider that swallowed the cause). Mirrors
// grading.runner's isInterruption; callers treat it as "record a plain
// attempt, write no terminal error column".
func isInterruption(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() != nil
}

// retryableError wraps transient failures so the queue retries (mirrors
// grading.runner's taxonomy).
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// nz returns the int64 value of a nullable, or 0 when null.
func nz(v pgtype.Int8) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

// ---- small pgtype helpers ----

// int8OrNull maps 0 to NULL, relying on the contract that every identity PK
// this is used with (students, actors, submissions, problems, ...) starts at
// 1 — 0 is never a valid row id, so it unambiguously means "absent".
func int8OrNull(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }
func textOrNull(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}
func int4Of(v int) pgtype.Int4  { return pgtype.Int4{Int32: int32(v), Valid: true} }
func boolOf(v bool) pgtype.Bool { return pgtype.Bool{Bool: v, Valid: true} }

// ---- zip / file helpers ----

// acceptZipEntry rejects directories, the macOS resource-fork tree, and
// dotfiles (design spec §5).
func acceptZipEntry(entry *zip.File) bool {
	name := entry.Name
	if entry.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
		return false
	}
	if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") {
		return false
	}
	base := baseName(name)
	if base == "" || strings.HasPrefix(base, ".") {
		return false
	}
	return true
}

func readZipEntry(entry *zip.File) ([]byte, error) {
	rc, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxEntryBytes+1))
}

// baseName strips any directory prefix (handling both "/" and "\" separators,
// since Windows-created zips carry backslash paths).
func baseName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// acceptedExt returns the lowercased extension (without the dot) and whether it
// is an accepted scan format: pdf/png/jpg/jpeg, case-insensitive (design spec §5).
func acceptedExt(filename string) (string, bool) {
	base := baseName(filename)
	i := strings.LastIndexByte(base, '.')
	if i <= 0 || i == len(base)-1 {
		return "", false
	}
	ext := strings.ToLower(base[i+1:])
	switch ext {
	case "pdf", "png", "jpeg":
		return ext, true
	case "jpg":
		return "jpg", true
	default:
		return "", false
	}
}

// openZip returns an io.ReaderAt over the stored zip plus its size and a
// cleanup func, using O(1) memory (F4). It prefers the blobstore's optional
// RandomAccess capability (LocalDisk hands back an *os.File); when the store
// does not implement it, it falls back to streaming Get → an os.CreateTemp
// file in os.TempDir, zip.NewReader over that temp file, and removes it on
// close. Either way the whole zip is never buffered into a []byte.
func (s *Service) openZip(ctx context.Context, key string) (io.ReaderAt, int64, func(), error) {
	if ra, ok := s.Blobs.(blobstore.RandomAccess); ok {
		f, size, err := ra.OpenRange(ctx, key)
		if err != nil {
			return nil, 0, nil, err
		}
		return f, size, func() { _ = f.Close() }, nil
	}

	// Fallback: stream Get into a temp file, then read it in place.
	rc, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(os.TempDir(), "adamarker-zip-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() {
		name := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	size, err := io.Copy(tmp, io.LimitReader(rc, MaxZipBytes+1))
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return tmp, size, cleanup, nil
}

// storeEntryBlob content-addresses one entry's bytes and streams them into blob
// storage, returning the key, sha, and source kind for the row insert that
// follows (F15: the blob Put happens OUTSIDE the DB transaction — the key is
// derived from the content SHA, so an orphaned blob from a later tx failure is
// harmless and a re-run stores identical bytes idempotently).
func (s *Service) storeEntryBlob(ctx context.Context, assessmentID, batchID int64, data []byte, ext string) (key, sha, kind string, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	kind = "pdf"
	if ext != "pdf" {
		kind = "image"
	}
	key = fmt.Sprintf("assessments/%d/scans/%d/%s.%s", assessmentID, batchID, sha[:16], ext)
	if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		return "", "", "", fmt.Errorf("scan: store entry: %w", err)
	}
	return key, sha, kind, nil
}
