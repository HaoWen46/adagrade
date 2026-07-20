-- name: ListStudents :many
SELECT * FROM students ORDER BY student_id;

-- name: GetStudentByExternalID :one
SELECT * FROM students WHERE student_id = $1;

-- name: UpsertStudent :one
INSERT INTO students (student_id, name, email)
VALUES ($1, $2, $3)
ON CONFLICT (student_id) DO UPDATE
    SET name = EXCLUDED.name, email = EXCLUDED.email, updated_at = now()
RETURNING *;

-- name: CountStudents :one
SELECT count(*) FROM students;

-- name: GetStudent :one
SELECT * FROM students WHERE id = $1;

-- name: SetStudentWithdrawn :one
UPDATE students
SET withdrawn_at = CASE WHEN sqlc.arg(withdrawn)::bool THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- SetStudentsWithdrawnBulk is the import-diff sync action (roster-lifecycle
-- plan 2026-07-10): one UPDATE for "Withdraw all N" / "Reinstate all N",
-- keyed by EXTERNAL student_id (the ids the bulk endpoints receive). Returns
-- the affected-row count; the handler validates unknown ids to a 400 BEFORE
-- calling this, so updated == len(ids) on success.
-- name: SetStudentsWithdrawnBulk :execrows
UPDATE students
SET withdrawn_at = CASE WHEN sqlc.arg(withdrawn)::bool THEN now() ELSE NULL END,
    updated_at = now()
WHERE student_id = ANY (sqlc.arg(student_ids)::text []);

-- ListActiveStudentIDs / ListWithdrawnStudentIDs feed the roster import diff
-- (external ids only — no PII columns): actives absent from the CSV are the
-- add/drop candidates, withdrawn ids present in the CSV are the retaker trap.
-- name: ListActiveStudentIDs :many
SELECT student_id FROM students WHERE withdrawn_at IS NULL ORDER BY student_id;

-- name: ListWithdrawnStudentIDs :many
SELECT student_id FROM students WHERE withdrawn_at IS NOT NULL ORDER BY student_id;

-- GetStudentBlockingArtifacts enumerates every artifact kind that references a
-- student row and must block a hard delete (roster-delete plan, B15: a typo'd
-- import or smoke-test row can never be removed via Withdraw, and it haunts
-- every assessment's publish coverage gate forever). This is the schema's full
-- REFERENCES students (id) surface as of migration 0036:
--   submissions.student_id            (0003, NOT NULL)
--   answers.student_id                (0003, NOT NULL) -- see has_graded_answers below
--   scan_pages.proposed_student_id    (0029)
--   scan_pages.assigned_student_id    (0029)
--   scan_pages.park_student_id        (0031)
--   publish_items.student_id          (0016, NOT NULL)
--   regrade_requests.student_id       (0017)
--   upload_quarantine.resolved_student_id (0005)
-- (scan_files/0010 also referenced students but was DROPped by 0029 — nothing
-- to check there.)
--
-- Bare answers — a MaterializeAnswers row (ingestion.sql EnsureAnswer) with no
-- ingested page and no grading record — are NOT listed as a blocker: they are
-- pre-created placeholder scaffolding for the whole roster, not evidence the
-- student did anything. AnswerIDsForAssessment/AnswerIDsForProblem (runs.sql)
-- only ever resolve a run's scope to answers that already have a page, so a
-- truly bare answer can never carry a grading_run_item or grading_record
-- either — has_graded_answers is a safe, sufficient proxy for "this student's
-- answers are real artifacts, not scaffolding."
-- name: GetStudentBlockingArtifacts :one
SELECT
    EXISTS (
        SELECT 1 FROM submissions WHERE student_id = sqlc.arg(student_id)::bigint
    ) AS has_submissions,
    EXISTS (
        SELECT 1 FROM answers a
        WHERE a.student_id = sqlc.arg(student_id)::bigint
          AND (
              EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)
              OR EXISTS (SELECT 1 FROM grading_records gr WHERE gr.answer_id = a.id)
          )
    ) AS has_graded_answers,
    EXISTS (
        SELECT 1 FROM scan_pages sp
        WHERE sp.proposed_student_id = sqlc.arg(student_id)::bigint
           OR sp.assigned_student_id = sqlc.arg(student_id)::bigint
           OR sp.park_student_id = sqlc.arg(student_id)::bigint
    ) AS has_scan_pages,
    EXISTS (
        SELECT 1 FROM publish_items WHERE student_id = sqlc.arg(student_id)::bigint
    ) AS has_publish_items,
    EXISTS (
        SELECT 1 FROM regrade_requests WHERE student_id = sqlc.arg(student_id)::bigint
    ) AS has_regrade_requests,
    EXISTS (
        SELECT 1 FROM upload_quarantine WHERE resolved_student_id = sqlc.arg(student_id)::bigint
    ) AS has_quarantine_resolutions;

-- DeleteBareAnswersForStudent removes only bare (no page, no grading record)
-- answer rows for a student — run immediately before DeleteStudent once
-- GetStudentBlockingArtifacts has confirmed nothing real references the row.
-- The NOT EXISTS guards are repeated here (not just trusted from the check
-- query) so this statement can never destroy a real artifact even if a future
-- caller invokes it without the guard query first.
-- name: DeleteBareAnswersForStudent :execrows
DELETE FROM answers a
WHERE a.student_id = $1
  AND NOT EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)
  AND NOT EXISTS (SELECT 1 FROM grading_records gr WHERE gr.answer_id = a.id);

-- name: DeleteStudent :execrows
DELETE FROM students WHERE id = $1;
