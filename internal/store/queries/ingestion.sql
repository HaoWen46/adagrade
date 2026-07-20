-- name: CreateSubmission :one
INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, source_kind, page_count, uploaded_by, problem_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetSubmission :one
SELECT * FROM submissions WHERE id = $1;

-- name: GetActiveSubmission :one
SELECT * FROM submissions
WHERE assessment_id = $1 AND student_id = $2 AND superseded_by IS NULL AND retracted_at IS NULL AND problem_id IS NULL;

-- name: GetActiveSubmissionForProblem :one
SELECT * FROM submissions
WHERE assessment_id = $1 AND student_id = $2 AND problem_id = $3 AND superseded_by IS NULL AND retracted_at IS NULL;

-- name: ListLiveSubmissionsForStudent :many
-- Every live submission for (assessment, student) — whole-assessment (problem_id
-- IS NULL) AND per-problem rows alike. Used when a whole-assessment upload must
-- supersede all coexisting submissions (spec §8).
SELECT * FROM submissions
WHERE assessment_id = $1 AND student_id = $2 AND superseded_by IS NULL AND retracted_at IS NULL
ORDER BY id;

-- name: SupersedeSubmission :exec
UPDATE submissions SET superseded_by = $2 WHERE id = $1;

-- name: RetractSubmission :exec
UPDATE submissions SET retracted_at = now() WHERE id = $1;

-- name: ListActiveSubmissions :many
SELECT s.*, st.student_id AS student_external_id, st.name AS student_name
FROM submissions s
JOIN students st ON st.id = s.student_id
WHERE s.assessment_id = $1 AND s.superseded_by IS NULL
ORDER BY st.student_id;

-- name: EnsureAnswer :one
INSERT INTO answers (assessment_id, student_id, problem_id)
VALUES ($1, $2, $3)
ON CONFLICT (assessment_id, student_id, problem_id) DO UPDATE SET updated_at = answers.updated_at
RETURNING *;

-- name: GetAnswer :one
SELECT * FROM answers WHERE id = $1;

-- name: GetAnswerByKey :one
SELECT * FROM answers WHERE assessment_id = $1 AND student_id = $2 AND problem_id = $3;

-- name: CountRecordsForStudentAssessment :one
SELECT count(*)
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
WHERE a.assessment_id = $1 AND a.student_id = $2;

-- name: CountRecordsForStudentProblem :one
SELECT count(*)
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
WHERE a.assessment_id = $1 AND a.student_id = $2 AND a.problem_id = $3;

-- name: CountPublishedForStudentAssessment :one
SELECT count(*) FROM answers
WHERE assessment_id = $1 AND student_id = $2 AND published_at IS NOT NULL;

-- name: CountPublishedForStudentProblem :one
SELECT count(*) FROM answers
WHERE assessment_id = $1 AND student_id = $2 AND problem_id = $3 AND published_at IS NOT NULL;

-- name: DeletePagesForStudentAssessment :exec
DELETE FROM answer_pages
WHERE answer_id IN (SELECT id FROM answers WHERE assessment_id = $1 AND student_id = $2);

-- name: DeletePagesBySubmission :exec
DELETE FROM answer_pages WHERE submission_id = $1;

-- name: DeletePagesForAnswer :exec
-- Clears one answer's pages regardless of which submission owns them, so a
-- per-problem promotion can supersede only that problem's coverage even when the
-- pages were positionally mapped by a still-live whole-assessment submission (§8).
DELETE FROM answer_pages WHERE answer_id = $1;

-- name: CreateAnswerPage :one
INSERT INTO answer_pages (answer_id, page_index, submission_id, pdf_page_index, image_ref, image_sha256, image_width, image_height, text_loss_runs)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListAnswerPages :many
SELECT * FROM answer_pages WHERE answer_id = $1 ORDER BY page_index;

-- name: GetAnswerPage :one
SELECT * FROM answer_pages WHERE id = $1;

-- name: NextPageIndex :one
SELECT COALESCE(MAX(page_index) + 1, 0)::int FROM answer_pages WHERE answer_id = $1;

-- name: MoveAnswerPage :one
UPDATE answer_pages SET answer_id = $2, page_index = $3 WHERE id = $1 RETURNING *;

-- name: AddAnswerFlag :exec
UPDATE answers SET flags = array_append(flags, sqlc.arg(flag)::text), updated_at = now()
WHERE id = $1 AND NOT (sqlc.arg(flag)::text = ANY (flags));

-- RemoveAnswerFlag is guarded the same way AddAnswerFlag is (F11): without the
-- ANY(flags) check, aggregation's per-answer "clear all agg_* flags, re-add
-- raised" loop (grading/aggregate_run.go) rewrites every answer row 3x per
-- pass even when nothing changed, stamping a fresh updated_at each time. The
-- guard makes the UPDATE match zero rows when the flag was never present.
-- name: RemoveAnswerFlag :exec
UPDATE answers SET flags = array_remove(flags, sqlc.arg(flag)::text), updated_at = now()
WHERE id = $1 AND sqlc.arg(flag)::text = ANY (flags);

-- name: AddFlagForStudentAssessment :exec
UPDATE answers SET flags = array_append(flags, sqlc.arg(flag)::text), updated_at = now()
WHERE assessment_id = $1 AND student_id = $2 AND NOT (sqlc.arg(flag)::text = ANY (flags));

-- name: AddFlagForStudentProblem :exec
UPDATE answers SET flags = array_append(flags, sqlc.arg(flag)::text), updated_at = now()
WHERE assessment_id = $1 AND student_id = $2 AND problem_id = $3 AND NOT (sqlc.arg(flag)::text = ANY (flags));

-- name: ListAnswersForAssessment :many
SELECT * FROM answers WHERE assessment_id = $1 ORDER BY student_id, problem_id;

-- name: MaterializeAnswers :exec
INSERT INTO answers (assessment_id, student_id, problem_id)
SELECT $1, st.id, p.id
FROM students st
CROSS JOIN problems p
WHERE p.assessment_id = $1 AND st.withdrawn_at IS NULL
ON CONFLICT DO NOTHING;

-- CountAnswersForAssessment backs the materialize-answers action's
-- `{"created": n}` response (roster-lifecycle plan 2026-07-10):
-- MaterializeAnswers is :exec (its ingest caller ignores the row count), so
-- the handler counts before/after inside the same transaction instead.
-- name: CountAnswersForAssessment :one
SELECT count(*) FROM answers WHERE assessment_id = $1;

-- name: CreateQuarantine :one
INSERT INTO upload_quarantine (assessment_id, original_filename, pdf_ref, pdf_sha256, reason, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListOpenQuarantine :many
SELECT * FROM upload_quarantine WHERE assessment_id = $1 AND resolved_at IS NULL ORDER BY id;

-- name: GetQuarantine :one
SELECT * FROM upload_quarantine WHERE id = $1;

-- name: ResolveQuarantine :exec
UPDATE upload_quarantine SET resolved_at = now(), resolved_student_id = $2 WHERE id = $1;

-- name: DismissQuarantine :execrows
UPDATE upload_quarantine
SET resolved_at = now(), resolved_student_id = NULL
WHERE id = $1 AND resolved_at IS NULL;

-- name: ReclassifyQuarantine :execrows
UPDATE upload_quarantine
SET reason = $2
WHERE id = $1 AND resolved_at IS NULL;

-- name: IngestReportRows :many
SELECT st.id AS student_db_id,
    st.student_id,
    st.name,
    s.id AS submission_id,
    s.page_count,
    s.original_filename,
    (SELECT count(*) FROM answer_pages ap
        JOIN answers a ON a.id = ap.answer_id
        WHERE a.assessment_id = $1 AND a.student_id = st.id) AS mapped_pages
FROM students st
LEFT JOIN submissions s
    ON s.assessment_id = $1 AND s.student_id = st.id AND s.superseded_by IS NULL AND s.retracted_at IS NULL AND s.problem_id IS NULL
WHERE st.withdrawn_at IS NULL
ORDER BY st.student_id;
