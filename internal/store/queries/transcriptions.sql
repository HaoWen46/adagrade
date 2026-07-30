-- name: GetAnswerTranscription :one
-- Content-addressed cache lookup: same pages, same model, same prompt, same
-- params => the transcription was already paid for.
SELECT *
FROM answer_transcriptions
WHERE answer_id = $1
  AND image_shas = $2
  AND model_id = $3
  AND prompt_version = $4
  AND params_hash = $5;

-- name: UpsertAnswerTranscription :one
-- ON CONFLICT DO UPDATE rather than DO NOTHING so a concurrent duplicate call
-- still returns the row (DO NOTHING returns no rows, which callers would have
-- to special-case).
INSERT INTO answer_transcriptions (
    answer_id, image_shas, model_id, prompt_version, params_hash,
    blocks, confidence, redaction_counts, input_tokens, output_tokens, cost_usd
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (answer_id, image_shas, model_id, prompt_version, params_hash)
DO UPDATE SET blocks = EXCLUDED.blocks
RETURNING *;

-- name: ListProblemAnswersForExport :many
-- Everything the export bundle needs for one (assessment, problem), ordered by
-- student id so the archive is deterministic. Students with no answer row are
-- absent by construction; the caller reconciles against the roster to tell
-- "wrote nothing" from "never scanned".
SELECT
    a.id            AS answer_id,
    s.student_id    AS student_id,
    s.name          AS student_name,
    s.email         AS student_email,
    a.flags         AS flags
FROM answers a
JOIN students s ON s.id = a.student_id
JOIN problems p ON p.id = a.problem_id
WHERE a.assessment_id = $1 AND p.number = $2
ORDER BY s.student_id;

-- name: ListAnswerPagesForExport :many
-- Masked page refs in page order. masked_image_ref is the ONLY image that may
-- leave the box (D10/D19), and mask_review_status rides along because the
-- export must hold grading's own line: a run is blocked while any page is
-- unmasked OR not human-accepted (masks.sql, the D10 gate), so the export may
-- not ship a pending-review mask to a provider either — pending means nobody
-- has confirmed the rectangle actually covers the identity.
SELECT
    ap.answer_id       AS answer_id,
    ap.page_index      AS page_index,
    ap.masked_image_ref AS masked_image_ref,
    ap.image_sha256    AS image_sha256,
    ap.mask_review_status AS mask_review_status
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
JOIN problems p ON p.id = a.problem_id
WHERE a.assessment_id = $1 AND p.number = $2
ORDER BY ap.answer_id, ap.page_index;

-- name: CountProblemTranscriptionStatus :many
-- Per-problem coverage for the export status panel: how many answers exist, how
-- many already have a cached transcription under the given contract, and how far
-- through mask review the problem's pages are.
--
-- The two page counts are what make the export's D10 mask gate a PRE-flight check
-- instead of a per-answer degradation discovered mid-bundle: a problem is
-- exportable only when every one of its answer_pages rows is human-accepted
-- (masks.sql, the same line grading's run gate holds). They are scalar subqueries
-- rather than another LEFT JOIN because joining answer_pages here would multiply
-- the answers/cached counts by the page count.
--
-- Whole-assessment page totals are the SUM of these columns: every answer_pages
-- row of an assessment hangs off an answer, and every answer hangs off exactly one
-- of this assessment's problems. The status handler derives gates.pages_total /
-- gates.pages_mask_accepted that way rather than paying for a second round trip.
--
-- GROUP BY p.id (not number/title) so the scalar subqueries may reference p.id:
-- grouping by the primary key makes every other problems column functionally
-- dependent, and it is also the honest grouping — one row per problem.
SELECT
    p.number AS problem_number,
    p.title  AS problem_title,
    COUNT(a.id) AS answers,
    COUNT(t.id) AS cached,
    coalesce((
        SELECT count(*)
        FROM answer_pages ap
        JOIN answers a2 ON a2.id = ap.answer_id
        WHERE a2.problem_id = p.id AND a2.assessment_id = p.assessment_id
    ), 0)::bigint AS pages,
    coalesce((
        SELECT count(DISTINCT ap.answer_id)
        FROM answer_pages ap
        JOIN answers a2 ON a2.id = ap.answer_id
        WHERE a2.problem_id = p.id AND a2.assessment_id = p.assessment_id
    ), 0)::bigint AS answers_with_pages,
    coalesce((
        SELECT count(*)
        FROM answer_pages ap
        JOIN answers a2 ON a2.id = ap.answer_id
        WHERE a2.problem_id = p.id AND a2.assessment_id = p.assessment_id
          AND ap.mask_review_status = 'accepted'
    ), 0)::bigint AS pages_mask_accepted
FROM problems p
LEFT JOIN answers a ON a.problem_id = p.id AND a.assessment_id = p.assessment_id
LEFT JOIN answer_transcriptions t
       ON t.answer_id = a.id
      AND t.model_id = $2
      AND t.prompt_version = $3
      AND t.params_hash = $4
WHERE p.assessment_id = $1
GROUP BY p.id
ORDER BY p.number;

-- name: CountAssessmentAnswerCoverage :one
-- The roster half of the export ladder's prerequisite gates.
--
-- students_total is the WHOLE roster, withdrawn rows included — the same count
-- CountStudents (students.sql) reports, and the same population the Students page
-- lists (D23: withdrawing hides nothing, it only labels). The gate is answering
-- "has a roster been loaded at all?", and a withdrawn student is still a loaded
-- roster row.
--
-- students_with_work counts DISTINCT students holding at least one answers row in
-- this assessment. answers rows are materialized per (assessment, student,
-- problem), so a plain count would multiply by the problem count.
SELECT
    (SELECT count(*) FROM students)::bigint AS students_total,
    (SELECT count(DISTINCT a.student_id) FROM answers a
      WHERE a.assessment_id = $1)::bigint AS students_with_work;
