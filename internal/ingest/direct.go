package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// DirectUploadInput is one bulk direct-upload file to stage (D27, F1). Kind selects
// the ingest decode path ("pdf" | "image"); Force permits replacing an
// already-graded student's submission (threaded through to Ingest); Actor is the
// uploading user.
type DirectUploadInput struct {
	Filename string
	Data     []byte
	Kind     string // "pdf" | "image"
	Force    bool
	Actor    int64
}

// StageDirectUpload validates and stages one bulk direct-upload file, then enqueues
// its ingest job off-request (D27, F1). Bulk direct upload previously ran the full
// ingest pipeline (render every page, guards, DB writes) for every file inside the
// HTTP request; at 200×9 scale that cannot complete. Now the request does only the
// cheap, synchronous part — the same size/emptiness gate the pipeline applies, then
// a content-addressed blob write and a direct_uploads row insert — and the heavy
// ingest runs in a worker (IngestDirectUpload).
//
// An empty or too-large file is rejected synchronously: it returns a non-empty
// rejectedReason and creates NO row (there is nothing to ingest, and a caller can
// report the rejection immediately). A valid file returns the new row id with an
// empty rejectedReason; the row insert and the ingest-job enqueue commit in one tx.
func (s *Service) StageDirectUpload(ctx context.Context, assessmentID int64, in DirectUploadInput) (id int64, rejectedReason string, err error) {
	if len(in.Data) == 0 || len(in.Data) > MaxPDFBytes {
		// Same gate ingest.Ingest applies, surfaced synchronously with no row.
		return 0, "file empty or too large", nil
	}

	sum := sha256.Sum256(in.Data)
	sha := hex.EncodeToString(sum[:])
	kind := in.Kind
	if kind != "image" {
		kind = "pdf"
	}
	ext := "pdf"
	if kind == "image" {
		ext = sniffImageExt(in.Data)
	}
	key := fmt.Sprintf("assessments/%d/uploads/%s.%s", assessmentID, sha[:16], ext)
	if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(in.Data)); err != nil {
		return 0, "", fmt.Errorf("ingest: stage direct upload: store blob: %w", err)
	}

	err = s.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		row, err := q.CreateDirectUpload(ctx, db.CreateDirectUploadParams{
			AssessmentID:     assessmentID,
			OriginalFilename: in.Filename,
			SourceRef:        key,
			SourceSha256:     sha,
			SourceKind:       kind,
			Force:            in.Force,
			UploadedBy:       pgtype.Int8{Int64: in.Actor, Valid: in.Actor != 0},
		})
		if err != nil {
			return err
		}
		id = row.ID
		if s.EnqueueDirectIngest != nil {
			return s.EnqueueDirectIngest(ctx, tx, []int64{id})
		}
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	return id, "", nil
}

// IngestDirectUpload is the direct-upload ingest job body (D27, F1). It runs the
// full ingest pipeline on a staged file's stored bytes off-request and records the
// FileResult back onto the row.
//
// It is idempotent under River redelivery: a row whose finished_at is already set is
// skipped (a re-delivered job must not create a second submission). Otherwise it
// stamps started_at, reads the blob, and runs Ingest with the row's actor + force.
// The ingest result — whether ingested, quarantined (unknown filename), or rejected
// (a business block such as graded-without-force) — is a RESULT, not an error: it is
// persisted via SetDirectUploadResult (status/reason/submission_id) and the job
// returns nil. A transient failure (the blob is unreadable) is returned so the queue
// retries; on finalAttempt it is recorded as a PII-free error column and finished, so
// the row does not silently wedge.
//
// F17: a shutdown/timeout that cancels Blobs.Get/ReadAll mid-flight surfaces as
// context.Canceled/DeadlineExceeded. This is an INTERRUPTION, not "source bytes
// unreadable" — never write that terminal error column for it, not even on the final
// attempt. isInterruption is checked BEFORE each finalAttempt branch below so a
// wrapped-away cancellation is still caught; the error is returned so River records a
// plain attempt (the worker snoozes it on shutdown) and the row is retried on restart
// instead of being wedged with a false terminal reason.
func (s *Service) IngestDirectUpload(ctx context.Context, id int64, finalAttempt bool) error {
	row, err := s.Store.Q.GetDirectUpload(ctx, id)
	if err != nil {
		return fmt.Errorf("ingest: direct upload: load %d: %w", id, err)
	}
	if row.FinishedAt.Valid {
		return nil // already done (idempotent redelivery)
	}
	if err := s.Store.Q.SetDirectUploadStarted(ctx, id); err != nil {
		return err
	}

	rc, err := s.Blobs.Get(ctx, row.SourceRef)
	if err != nil {
		if isInterruption(ctx, err) {
			s.log().Warn("ingest: direct upload interrupted (shutdown/timeout); not terminal", "id", id)
			return err
		}
		if finalAttempt {
			return s.Store.Q.SetDirectUploadResult(ctx, db.SetDirectUploadResultParams{
				ID: id, Error: pgtype.Text{String: "source bytes unreadable", Valid: true},
			})
		}
		return fmt.Errorf("ingest: direct upload %d: read blob: %w", id, err) // transient: retry
	}
	data, err := io.ReadAll(io.LimitReader(rc, MaxPDFBytes+1))
	rc.Close()
	if err != nil {
		if isInterruption(ctx, err) {
			s.log().Warn("ingest: direct upload interrupted (shutdown/timeout); not terminal", "id", id)
			return err
		}
		if finalAttempt {
			return s.Store.Q.SetDirectUploadResult(ctx, db.SetDirectUploadResultParams{
				ID: id, Error: pgtype.Text{String: "source bytes unreadable", Valid: true},
			})
		}
		return fmt.Errorf("ingest: direct upload %d: read bytes: %w", id, err) // transient: retry
	}

	res := s.Ingest(ctx, row.AssessmentID, IngestInput{
		Filename: row.OriginalFilename,
		Data:     data,
		Kind:     row.SourceKind,
	}, nz8(row.UploadedBy), row.Force)

	// The ingest outcome (ingested / quarantined / rejected) is a RESULT to record,
	// not an error to retry — a business block is a terminal per-file outcome.
	return s.Store.Q.SetDirectUploadResult(ctx, db.SetDirectUploadResultParams{
		ID:           id,
		Status:       pgtype.Text{String: res.Status, Valid: res.Status != ""},
		Reason:       pgtype.Text{String: res.Reason, Valid: res.Reason != ""},
		SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: res.SubmissionID != 0},
	})
}

// nz8 returns the int64 value of a nullable, or 0 when null (0 = "no actor" for
// ingest's uploadedBy).
func nz8(v pgtype.Int8) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
