-- name: ListMaskRegions :many
SELECT * FROM mask_regions WHERE assessment_id = $1 ORDER BY id;

-- name: DeleteMaskRegions :exec
DELETE FROM mask_regions WHERE assessment_id = $1;

-- name: CreateMaskRegion :one
INSERT INTO mask_regions (assessment_id, page_scope, x, y, w, h, color, padding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListPagesForAssessment :many
SELECT ap.*, a.assessment_id, a.student_id, a.problem_id
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
WHERE a.assessment_id = $1
ORDER BY ap.answer_id, ap.page_index;

-- SetPageMasked records the freshly-masked artifact and the fingerprint of the
-- inputs that produced it (mask_input_sha — original sha + quality + region set),
-- and resets the page's mask review to pending (D10). The fingerprint lets a
-- re-apply job skip pages already up to date and preserve their review status
-- (D27, F2): only pages whose inputs actually changed are re-masked + re-reset.
-- It also clears mask_error (D27 review, F1): a successful (re-)mask is the
-- recovery from an earlier terminal decode failure.
-- name: SetPageMasked :one
UPDATE answer_pages
SET masked_image_ref = $2, mask_input_sha = $3, masked_at = now(),
    mask_review_status = 'pending', mask_reviewed_by = NULL, mask_reviewed_at = NULL,
    mask_error = NULL
WHERE id = $1
RETURNING *;

-- SetPageMaskError records a short, PII-FREE terminal error on a page whose mask
-- job exhausted its River attempts on a deterministic failure (D27 review, F1),
-- so the D10 run gate and the review UI can show WHY the page never masked instead
-- of blocking silently. The message is a static reason category only — never a
-- dynamic string that could carry a path or student content.
-- name: SetPageMaskError :exec
UPDATE answer_pages SET mask_error = $2 WHERE id = $1;

-- name: SetMaskReview :one
UPDATE answer_pages
SET mask_review_status = $2, mask_reviewed_by = $3, mask_reviewed_at = now()
WHERE id = $1
RETURNING *;

-- name: MaskReviewList :many
SELECT ap.id, ap.answer_id, ap.page_index, ap.masked_image_ref, ap.mask_review_status,
    ap.mask_error, st.student_id, p.number AS problem_number
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
WHERE a.assessment_id = $1
ORDER BY p.number, st.student_id, ap.page_index;

-- AcceptPendingMasks bulk-accepts every masked page of an assessment whose
-- review is still pending — the "spot-checked a few, accept the rest" path.
-- Unmasked pages (no derivative to review) and flagged pages are untouched.
-- name: AcceptPendingMasks :execrows
UPDATE answer_pages ap
SET mask_review_status = 'accepted', mask_reviewed_by = $2, mask_reviewed_at = now()
FROM answers a
JOIN problems p ON p.id = a.problem_id
WHERE ap.answer_id = a.id AND p.assessment_id = $1
  AND ap.mask_review_status = 'pending' AND ap.masked_image_ref IS NOT NULL;

-- name: CountMaskBlockers :one
SELECT count(*)
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
WHERE a.assessment_id = $1
  AND (ap.masked_image_ref IS NULL OR ap.mask_review_status <> 'accepted');

-- ListAcceptedMaskPages feeds the stale-mask reconciliation (stale-mask fix
-- 2026-07-11): the review-ACCEPTED pages of an assessment with just the columns
-- ingest.StaleAcceptedMasks needs to recompute each page's mask fingerprint
-- (MaskFingerprint is Go-side sha256, so the comparison can't live in SQL).
-- Accepted pages are the only ones that matter here — pending/flagged/unmasked
-- pages already fail the CountMaskBlockers gates.
-- name: ListAcceptedMaskPages :many
SELECT ap.id, ap.image_sha256, ap.pdf_page_index, ap.masked_image_ref, ap.mask_input_sha
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
WHERE a.assessment_id = $1 AND ap.mask_review_status = 'accepted';

-- ResetStaleMaskReview knocks accepted pages back to pending (clearing the
-- reviewer stamp) when their masked artifact was produced from inputs that no
-- longer match the current region set (stale-mask fix 2026-07-11): without this,
-- a region edit after review acceptance leaves the old — possibly identity-
-- revealing — masked images flowing to providers forever, because the grading
-- gates only check "masked + accepted". The accepted-only guard makes the reset
-- idempotent and keeps it from touching flagged pages (already blockers).
-- name: ResetStaleMaskReview :execrows
UPDATE answer_pages
SET mask_review_status = 'pending', mask_reviewed_by = NULL, mask_reviewed_at = NULL
WHERE id = ANY (sqlc.arg(page_ids)::bigint [])
  AND mask_review_status = 'accepted';
