-- Direct-upload staging (D27; audit finding F1). A bulk direct upload stages one
-- row per file synchronously (size/emptiness checked, blob stored) and enqueues an
-- ingest job; the worker runs the ingest pipeline off-request and records the
-- FileResult back onto the row. status/reason/submission_id mirror
-- ingest.FileResult (never logged — reason/filename can carry PII context, D14).

-- name: CreateDirectUpload :one
INSERT INTO direct_uploads (assessment_id, original_filename, source_ref, source_sha256, source_kind, force, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetDirectUpload :one
SELECT * FROM direct_uploads WHERE id = $1;

-- SetDirectUploadStarted stamps started_at so the reconciliation view can show the
-- job as in-flight. Idempotent under redelivery: re-stamping is harmless.
-- name: SetDirectUploadStarted :exec
UPDATE direct_uploads SET started_at = now() WHERE id = $1;

-- SetDirectUploadResult records the terminal FileResult (status/reason/submission_id
-- for a completed ingest, or error for a transient failure that exhausted retries)
-- and stamps finished_at, which flips the row out of the pending set.
-- name: SetDirectUploadResult :exec
UPDATE direct_uploads
SET status = $2, reason = $3, submission_id = $4, error = $5, finished_at = now()
WHERE id = $1;

-- name: ListDirectUploadsForAssessment :many
SELECT * FROM direct_uploads WHERE assessment_id = $1 ORDER BY id DESC LIMIT $2;
