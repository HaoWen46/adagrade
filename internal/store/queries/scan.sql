-- Page-level scan intake queries (design spec 2026-07-04). All OCR text columns
-- hold PII (D14): callers select them only into DB-bound rows or staff-facing
-- JSON, never into logs, job args, or error strings.

-- ---- batches ----

-- name: CreateScanBatch :one
INSERT INTO scan_batches (assessment_id, ocr_enabled, ocr_provider, ocr_model, source_ref, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetScanBatch :one
SELECT * FROM scan_batches WHERE id = $1;

-- name: ListScanBatches :many
SELECT * FROM scan_batches WHERE assessment_id = $1 ORDER BY id DESC;

-- name: SetBatchSourceRef :exec
UPDATE scan_batches SET source_ref = $2 WHERE id = $1;

-- SetScanBatchOCR repoints a batch's cloud OCR provider/model — the recovery
-- path for a batch created against a provider that is now gone/disabled
-- (retry-errored); the batch's provider is otherwise immutable.
-- name: SetScanBatchOCR :exec
UPDATE scan_batches SET ocr_provider = $2, ocr_model = $3 WHERE id = $1;

-- ---- sources ----

-- CreateScanSource is idempotent within a batch: UNIQUE(batch_id, source_sha256)
-- collapses duplicate uploads; ON CONFLICT DO NOTHING returns no row for a
-- duplicate (sqlc :one surfaces pgx.ErrNoRows) — caller treats as "skipped".
-- name: CreateScanSource :one
INSERT INTO scan_sources (batch_id, original_filename, source_ref, source_sha256, source_kind)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (batch_id, source_sha256) DO NOTHING
RETURNING *;

-- name: GetScanSource :one
SELECT * FROM scan_sources WHERE id = $1;

-- name: ListScanSourcesForBatch :many
SELECT * FROM scan_sources WHERE batch_id = $1 ORDER BY id;

-- name: SetScanSourcePageCount :exec
UPDATE scan_sources SET page_count = $2, error = NULL WHERE id = $1;

-- name: SetScanSourceError :exec
UPDATE scan_sources SET error = $2 WHERE id = $1;

-- ---- pages: creation & pipeline writes ----

-- CreateScanPage is idempotent per source page (UNIQUE(source_id, page_index)):
-- a redelivered scan.split re-inserts harmlessly.
-- name: CreateScanPage :one
INSERT INTO scan_pages (source_id, batch_id, assessment_id, page_index)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source_id, page_index) DO NOTHING
RETURNING *;

-- name: GetScanPage :one
SELECT * FROM scan_pages WHERE id = $1;

-- GetScanPageForUpdate re-reads a page INSIDE a mutating transaction, locking
-- the row: the manual mutations' promoted-guard runs first on a plain pre-tx
-- snapshot, and an in-flight promote job can link a submission between that
-- snapshot and the mutation's own writes. Re-checking on the locked row turns
-- that race into a clean 409 instead of silently mutating a promoted page.
-- name: GetScanPageForUpdate :one
SELECT * FROM scan_pages WHERE id = $1 FOR UPDATE;

-- name: ListScanPagesForSource :many
SELECT * FROM scan_pages WHERE source_id = $1 ORDER BY page_index;

-- name: SetScanPageRendered :exec
UPDATE scan_pages
SET image_ref = $2, image_sha256 = $3, image_width = $4, image_height = $5,
    student_id_crop_ref = $6, name_crop_ref = $7, problem_crop_ref = $8,
    text_loss_runs = $9,
    error = NULL, updated_at = now()
WHERE id = $1;

-- SetScanPageIdentified records the OCR reads + proposal and stamps
-- identified_at — the orphan/processing boundary for derived state.
-- name: SetScanPageIdentified :exec
UPDATE scan_pages
SET ocr_student_id = $2, ocr_name = $3, ocr_problem = $4,
    ocr_student_id_legible = $5, ocr_name_legible = $6, ocr_problem_legible = $7,
    ocr_engine = $8,
    proposed_student_id = $9, proposed_problem_id = $10, proposal_source = $11,
    identified_at = now(), error = NULL, updated_at = now()
WHERE id = $1;

-- ---- pages: assignment & lifecycle ----

-- AssignScanPage clears parked/discard/error state: assignment (auto or manual)
-- always wins over a stale park or error, and a TA who assigns by eye after an
-- OCR outage must not be blocked by a lingering error.
-- name: AssignScanPage :exec
UPDATE scan_pages
SET assigned_student_id = $2, assigned_problem_id = $3, assigned_by = $4,
    assigned_at = now(), parked_reason = NULL, parked_against = NULL,
    park_student_id = NULL, park_problem_id = NULL,
    discarded_at = NULL, discard_reason = NULL, error = NULL, updated_at = now()
WHERE id = $1;

-- name: UnassignScanPage :exec
UPDATE scan_pages
SET assigned_student_id = NULL, assigned_problem_id = NULL, assigned_by = NULL,
    assigned_at = NULL, force_promote = FALSE, updated_at = now()
WHERE id = $1;

-- ParkScanPage records the CONTESTED cell (park_student_id/park_problem_id)
-- alongside the incumbent so ResolveConflict's replace can target the cell
-- where the fight happened even after the incumbent moves (see 0031).
-- name: ParkScanPage :exec
UPDATE scan_pages
SET parked_reason = $2, parked_against = $3,
    park_student_id = $4, park_problem_id = $5, updated_at = now()
WHERE id = $1;

-- name: SetScanPageForcePromote :exec
UPDATE scan_pages SET force_promote = $2, updated_at = now() WHERE id = $1;

-- name: DiscardScanPage :exec
UPDATE scan_pages
SET discarded_at = now(), discard_reason = $2, parked_reason = NULL,
    parked_against = NULL, park_student_id = NULL, park_problem_id = NULL,
    error = NULL, updated_at = now()
WHERE id = $1;

-- name: UndiscardScanPage :exec
UPDATE scan_pages
SET discarded_at = NULL, discard_reason = NULL, updated_at = now()
WHERE id = $1;

-- name: SetScanPageError :exec
UPDATE scan_pages SET error = $2, updated_at = now() WHERE id = $1;

-- SetScanPagePromotionError stamps a promote-job failure ONLY on a
-- still-unpromoted row (same race guard as the old SetScanFilePromotionError):
-- a losing duplicate promote job must not stamp an error onto a promoted row.
-- name: SetScanPagePromotionError :exec
UPDATE scan_pages SET error = $2, updated_at = now() WHERE id = $1 AND submission_id IS NULL;

-- name: ClearScanPageError :exec
UPDATE scan_pages SET error = NULL, updated_at = now() WHERE id = $1;

-- ListErroredPagesForBatch feeds the batch-level bulk recovery actions
-- (retry-errored / discard-errored): every page of the batch currently in
-- derived state "error" (error is the highest-precedence state, D2).
-- name: ListErroredPagesForBatch :many
SELECT * FROM scan_pages
WHERE batch_id = $1 AND error IS NOT NULL AND error <> ''
ORDER BY id;

-- name: SetScanPageSubmission :exec
UPDATE scan_pages SET submission_id = $2, updated_at = now() WHERE id = $1;

-- LinkScanPagePromotion is the promote job's success-path link, run INSIDE the
-- same transaction that creates the submission (ingest's LinkInTx seam). It
-- links ONLY when the page still carries the exact assignment the promote job
-- read and is still unpromoted and undiscarded: a concurrent reassign/
-- unassign/discard makes this 0 rows, which aborts the whole promote
-- transaction — the wrong-cell submission never becomes visible. Mirrors the
-- error-path race guard on SetScanPagePromotionError.
-- name: LinkScanPagePromotion :execrows
UPDATE scan_pages
SET submission_id = $2, updated_at = now()
WHERE id = $1 AND submission_id IS NULL AND discarded_at IS NULL
  AND assigned_student_id = $3 AND assigned_problem_id = $4;

-- ClearPromotionErrorsForAssessment lets a Finalize re-run re-attempt pages whose
-- error came from a previous promotion ("promotion rejected: " prefix) without
-- clearing genuine render/OCR errors.
-- name: ClearPromotionErrorsForAssessment :exec
UPDATE scan_pages
SET error = NULL, updated_at = now()
WHERE assessment_id = $1
  AND assigned_student_id IS NOT NULL
  AND submission_id IS NULL
  AND discarded_at IS NULL
  AND error LIKE 'promotion rejected:%';

-- ---- cell occupancy ----

-- LivePageForCell finds the live (non-discarded) page already assigned to this
-- exact (student, problem) cell, excluding the page being placed. Promoted pages
-- count — their assignment is what the submission came from.
-- name: LivePageForCell :one
SELECT * FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id = $2 AND assigned_problem_id = $3
  AND discarded_at IS NULL AND id <> $4
ORDER BY id LIMIT 1;

-- ---- listing / progress / matrix ----

-- ScanPageRows joins proposed/assigned student externals for staff-facing JSON
-- (PII to an authenticated TA+ session only).
-- name: ScanPageRows :many
SELECT sp.*,
    ps.student_id AS proposed_external_id, ps.name AS proposed_name_roster,
    a.student_id AS assigned_external_id, a.name AS assigned_name
FROM scan_pages sp
LEFT JOIN students ps ON ps.id = sp.proposed_student_id
LEFT JOIN students a ON a.id = sp.assigned_student_id
WHERE sp.assessment_id = $1
ORDER BY sp.id;

-- ScanBatchPageProgress mirrors the derived-state precedence exactly, each
-- bucket excluding every higher-precedence condition (D2; F6: no PII columns).
-- name: ScanBatchPageProgress :many
SELECT batch_id,
    count(*) AS total,
    count(*) FILTER (WHERE error IS NOT NULL AND error <> '') AS errored,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NOT NULL
    ) AS discarded,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NOT NULL
    ) AS promoted,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NOT NULL
    ) AS parked,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NOT NULL
    ) AS assigned,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NULL
          AND identified_at IS NOT NULL
    ) AS orphaned,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NULL
          AND identified_at IS NULL
    ) AS processing
FROM scan_pages
WHERE batch_id = ANY (sqlc.arg(batch_ids)::bigint [])
GROUP BY batch_id;

-- name: ListLiveAssignedPagesForAssessment :many
SELECT id, assigned_student_id, assigned_problem_id, assigned_by, submission_id
FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id IS NOT NULL AND discarded_at IS NULL;

-- Live submissions for the matrix's "covered by a submission" cells (whole-
-- assessment rows cover every problem for that student).
-- name: ListLiveSubmissionsForAssessment :many
SELECT id, student_id, problem_id FROM submissions
WHERE assessment_id = $1 AND superseded_by IS NULL AND retracted_at IS NULL;

-- ---- finalize / promote ----

-- name: ListAssignedUnpromotedPages :many
SELECT * FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id IS NOT NULL
  AND discarded_at IS NULL AND submission_id IS NULL
ORDER BY id;

-- A cell is missing when an active student × problem has neither a live assigned
-- page nor a live submission covering it (per-problem, or whole-assessment).
-- Count only in the finalize gate (PII discipline).
-- name: CountMissingCells :one
SELECT count(*)
FROM students st
CROSS JOIN problems p
WHERE p.assessment_id = sqlc.arg(assessment_id)
  AND st.withdrawn_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM scan_pages sp
    WHERE sp.assessment_id = sqlc.arg(assessment_id)
      AND sp.assigned_student_id = st.id AND sp.assigned_problem_id = p.id
      AND sp.discarded_at IS NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM submissions s
    WHERE s.assessment_id = sqlc.arg(assessment_id)
      AND s.student_id = st.id
      AND s.superseded_by IS NULL AND s.retracted_at IS NULL
      AND (s.problem_id IS NULL OR s.problem_id = p.id)
  );

-- name: ListMissingCells :many
SELECT st.student_id, st.name, p.number AS problem_number
FROM students st
CROSS JOIN problems p
WHERE p.assessment_id = sqlc.arg(assessment_id)
  AND st.withdrawn_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM scan_pages sp
    WHERE sp.assessment_id = sqlc.arg(assessment_id)
      AND sp.assigned_student_id = st.id AND sp.assigned_problem_id = p.id
      AND sp.discarded_at IS NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM submissions s
    WHERE s.assessment_id = sqlc.arg(assessment_id)
      AND s.student_id = st.id
      AND s.superseded_by IS NULL AND s.retracted_at IS NULL
      AND (s.problem_id IS NULL OR s.problem_id = p.id)
  )
ORDER BY st.student_id, p.number;

-- ---- id_regions (typed: one region per kind, applied to every page) ----

-- name: ListIDRegions :many
SELECT * FROM id_regions WHERE assessment_id = $1 ORDER BY id;

-- name: DeleteIDRegions :exec
DELETE FROM id_regions WHERE assessment_id = $1;

-- name: CreateIDRegion :one
INSERT INTO id_regions (assessment_id, kind, x, y, w, h, color, padding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- ---- roster ----

-- ListActiveStudents is the matching candidate set: withdrawn students are never
-- proposed (D23).
-- name: ListActiveStudents :many
SELECT * FROM students WHERE withdrawn_at IS NULL ORDER BY student_id;

-- name: ListWithdrawnStudents :many
SELECT id, student_id, name FROM students WHERE withdrawn_at IS NOT NULL ORDER BY student_id;
