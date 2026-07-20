-- name: CreateRun :one
INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, execution_mode, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRun :one
SELECT * FROM grading_runs WHERE id = $1;

-- name: GetRunForUpdate :one
SELECT * FROM grading_runs WHERE id = $1 FOR UPDATE;

-- name: IsFinalRunSelected :one
SELECT EXISTS (SELECT 1 FROM assessments WHERE final_run_id = $1);

-- ListRuns is the runs-page list; the narg filters are the TA-facing
-- assessment/status dropdowns (NULL = no filter), applied here rather than
-- client-side because the list is capped at row_limit rows.
-- name: ListRuns :many
SELECT r.*, m.name AS method_name, mv.version AS method_version, a.name AS assessment_name
FROM grading_runs r
JOIN grading_method_versions mv ON mv.id = r.method_version_id
JOIN grading_methods m ON m.id = mv.method_id
JOIN assessments a ON a.id = r.assessment_id
WHERE (sqlc.narg(assessment_id)::bigint IS NULL OR r.assessment_id = sqlc.narg(assessment_id))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
ORDER BY r.id DESC
LIMIT sqlc.arg(row_limit);

-- name: SetRunStatus :one
UPDATE grading_runs
SET status = $2,
    error = sqlc.narg(error),
    started_at = CASE WHEN $2 = 'running' AND started_at IS NULL THEN now() ELSE started_at END,
    finished_at = CASE WHEN $2 IN ('completed', 'cancelled', 'failed') THEN now() ELSE finished_at END
WHERE id = $1
RETURNING *;

-- name: MaybeCompleteRun :execrows
UPDATE grading_runs
SET status = 'completed', finished_at = now()
WHERE grading_runs.id = $1 AND status = 'running'
  AND NOT EXISTS (SELECT 1 FROM grading_run_items i WHERE i.run_id = $1 AND i.state IN ('pending', 'running'));

-- name: CreateRunItem :one
INSERT INTO grading_run_items (run_id, answer_id, model_id, provider, rubric_version_id, reference_solution_version_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (run_id, answer_id, model_id) DO NOTHING
RETURNING *;

-- name: GetRunItem :one
SELECT * FROM grading_run_items WHERE id = $1;

-- ListRunItems returns every item for the run, capped at sqlc.arg(item_limit)
-- (F20: the run-detail poll must not pull an unbounded ~1800-row table by
-- default; handleGetRun uses this only for the explicit `?all=1` view and
-- echoes a truncated flag when the cap is hit). Callers that need the true
-- unfiltered set for driving logic (not HTTP responses) pass a high cap.
-- name: ListRunItems :many
SELECT i.*, st.student_id, p.number AS problem_number
FROM grading_run_items i
JOIN answers a ON a.id = i.answer_id
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
WHERE i.run_id = $1
ORDER BY i.id
LIMIT sqlc.arg(item_limit);

-- ListRunItemsInteresting is handleGetRun's default (F20): only items a TA
-- would act on (failed needs retry, running is the live edge) plus the
-- GROUP-BY counts already cover the aggregate progress bar. Capped the same
-- way as ListRunItems for symmetry, though in practice failed+running is
-- always a small slice of a run.
-- name: ListRunItemsInteresting :many
SELECT i.*, st.student_id, p.number AS problem_number
FROM grading_run_items i
JOIN answers a ON a.id = i.answer_id
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
WHERE i.run_id = $1 AND i.state IN ('failed', 'running')
ORDER BY i.id
LIMIT sqlc.arg(item_limit);

-- name: MarkItemRunning :one
UPDATE grading_run_items
SET state = 'running', attempts = attempts + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkItemTerminal :one
UPDATE grading_run_items
SET state = $2, error = sqlc.narg(error), record_id = sqlc.narg(record_id), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ResetFailedItems :many
UPDATE grading_run_items
SET state = 'pending', error = NULL, updated_at = now()
WHERE run_id = $1 AND state = 'failed'
RETURNING id;

-- name: RunItemStateCounts :many
SELECT state, count(*) AS n FROM grading_run_items WHERE run_id = $1 GROUP BY state;

-- AnswerIDsForAssessment / AnswerIDsForProblem resolve a NEW run's scope to
-- answer ids. Withdrawn students are excluded (roster-lifecycle plan
-- 2026-07-10): launching a run after a 停修 must not spend tokens grading a
-- student who will never receive results. Items of an already-launched run
-- are untouched — the scope is resolved once at launch.
-- name: AnswerIDsForAssessment :many
SELECT a.id FROM answers a
JOIN students st ON st.id = a.student_id
WHERE a.assessment_id = $1
  AND st.withdrawn_at IS NULL
  AND EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)
ORDER BY a.id;

-- name: AnswerIDsForProblem :many
SELECT a.id FROM answers a
JOIN students st ON st.id = a.student_id
WHERE a.problem_id = $1
  AND st.withdrawn_at IS NULL
  AND EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)
ORDER BY a.id;

-- name: AnswersWithProblems :many
SELECT id, problem_id FROM answers WHERE id = ANY (sqlc.arg(answer_ids)::bigint []) ORDER BY id;

-- name: CountMaskBlockersForAnswers :one
SELECT count(*)
FROM answer_pages ap
WHERE ap.answer_id = ANY (sqlc.arg(answer_ids)::bigint [])
  AND (ap.masked_image_ref IS NULL OR ap.mask_review_status <> 'accepted');

-- name: InsertModelRecord :one
INSERT INTO grading_records (
    answer_id, run_id, source, provider, model_id, method_version_id,
    rubric_version_id, reference_solution_version_id, prompt_template_version_id,
    graded_image_shas, criterion_scores, total, comment, transcription, confidence,
    adjustments, raw_output, input_tokens, output_tokens, cost_usd, temperature, policy
)
VALUES ($1, $2, 'model', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
ON CONFLICT (run_id, answer_id, model_id) WHERE run_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetRecordForLeaf :one
SELECT * FROM grading_records
WHERE run_id = $1 AND answer_id = $2 AND model_id = $3;

-- (SetOfficialForRunUnflagged removed in 0027: officials are derived from the
-- assessment's final source by RecomputeOfficials — runs never set officials.)

-- CountSucceededItemsForRun backs the final-source zero-leaf guard (A3): a
-- completed run whose leaves are ALL failed/skipped can never produce a
-- spot-check sample (createSpotCheckSample only pools state='succeeded'
-- items), which would wedge publish behind an unreachable "review spot-check"
-- call to action. Same state definition as RunItemStateCounts' "succeeded"
-- bucket (the picker's "N succeeded" figure), so this check and that display
-- can never disagree.
-- name: CountSucceededItemsForRun :one
SELECT count(*) FROM grading_run_items WHERE run_id = $1 AND state = 'succeeded';

-- LatestCompletedRunForMethod anchors the publish-time spot-check gate (0027):
-- when the final source is a single method, its most recent completed run's
-- sample must be reviewed or waived before results can go out.
-- name: LatestCompletedRunForMethod :one
SELECT r.*
FROM grading_runs r
JOIN grading_method_versions mv ON mv.id = r.method_version_id
WHERE r.assessment_id = $1
  AND mv.method_id = $2
  AND r.status = 'completed'
ORDER BY r.id DESC
LIMIT 1;
