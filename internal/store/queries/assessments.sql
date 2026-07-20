-- name: CreateAssessment :one
INSERT INTO assessments (kind, name, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetAssessment :one
SELECT * FROM assessments WHERE id = $1;

-- Serializes final-source, publish, and retry transitions for one assessment.
-- Every state-changing caller takes the assessment lock before any run lock.
-- name: GetAssessmentForUpdate :one
SELECT * FROM assessments WHERE id = $1 FOR UPDATE;

-- name: ListAssessments :many
SELECT a.*,
    (SELECT count(*) FROM problems p WHERE p.assessment_id = a.id) AS problem_count
FROM assessments a
WHERE (sqlc.arg(include_archived)::bool OR a.archived_at IS NULL)
ORDER BY a.created_at DESC;

-- name: RenameAssessment :one
UPDATE assessments SET name = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetAssessmentArchived :one
UPDATE assessments
SET archived_at = CASE WHEN sqlc.arg(archived)::bool THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- SetAssessmentFinalSource records the exam-wide grading source (round-based
-- grading, 0027/0035): kind 'method' carries an exact run plus its derived
-- method id; 'consensus'/NULL carry neither (DB CHECK enforces).
-- name: SetAssessmentFinalSource :one
UPDATE assessments
SET final_source_kind = sqlc.narg(kind),
    final_method_id = sqlc.narg(method_id),
    final_run_id = sqlc.narg(run_id),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetAssessmentRegradeDeadline :one
UPDATE assessments
SET regrade_deadline = sqlc.narg(deadline),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteAssessment :exec
DELETE FROM assessments WHERE id = $1;

-- name: CountAssessmentSubmissions :one
SELECT count(*) FROM submissions WHERE assessment_id = $1;

-- name: CountAssessmentRecords :one
SELECT count(*)
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
WHERE a.assessment_id = $1;
