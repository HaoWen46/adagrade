-- name: GetAggregationPolicy :one
SELECT * FROM aggregation_policies WHERE assessment_id = $1;

-- name: UpsertAggregationPolicy :one
INSERT INTO aggregation_policies (assessment_id, method_version_ids, combiner, fault_tolerance, flag_triggers, set_official, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (assessment_id) DO UPDATE SET
    method_version_ids = EXCLUDED.method_version_ids,
    combiner = EXCLUDED.combiner,
    fault_tolerance = EXCLUDED.fault_tolerance,
    flag_triggers = EXCLUDED.flag_triggers,
    set_official = EXCLUDED.set_official,
    updated_by = EXCLUDED.updated_by,
    updated_at = now()
RETURNING *;

-- Latest usable model record per (answer, panel method version) across the
-- assessment: non-NULL total, and rubric_version = the problem's current latest
-- (D17/B-H20). Ordered for easy per-answer grouping in Go.
-- name: PanelRecordsForAssessment :many
SELECT DISTINCT ON (a.id, gr.method_version_id)
    a.id AS answer_id,
    gr.id AS record_id,
    gr.method_version_id,
    gr.criterion_scores,
    gr.total,
    gr.confidence,
    gr.rubric_version_id,
    rv.score_increment
FROM answers a
JOIN grading_records gr ON gr.answer_id = a.id
JOIN rubric_versions rv ON rv.id = gr.rubric_version_id
WHERE a.assessment_id = $1
  AND gr.source = 'model'
  AND gr.total IS NOT NULL
  AND gr.method_version_id = ANY (sqlc.arg(method_version_ids)::bigint [])
  AND gr.rubric_version_id = (
      SELECT rv2.id FROM rubric_versions rv2
      WHERE rv2.problem_id = a.problem_id
      ORDER BY rv2.version DESC LIMIT 1)
ORDER BY a.id, gr.method_version_id, gr.id DESC;

-- name: InsertAggregateRecord :one
INSERT INTO grading_records (
    answer_id, source, rubric_version_id, criterion_scores, total, comment,
    adjustments, raw_output, created_by
)
VALUES ($1, 'aggregate', $2, $3, $4, $5, '[]'::jsonb, $6, $7)
RETURNING *;
