-- name: InsertHumanRecord :one
INSERT INTO grading_records (answer_id, source, rubric_version_id, graded_image_shas, criterion_scores, total, comment, adjustments, created_by)
VALUES ($1, 'human', $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- The official pointer is DERIVED (round-based grading, migration 0027): the
-- assessment's chosen final source decides every answer; a human record applies
-- ONLY where the source is silent. Nothing else may write official_record_id.
--
-- Rule per unpublished answer:
--   source record = latest record on the problem's CURRENT rubric version with a
--     total, from the chosen source (kind='method': source='model' from the exact
--     pinned run; kind='consensus': source='aggregate') — but only when the
--     answer carries NO flags (any flag, agg_* or manual, blocks the AI source
--     and demands human eyes);
--   fallback      = latest human record on the current rubric;
--   official      = source record if present, else fallback, else NULL (a hole —
--     surfaced red on the publish preview until a human grades it).
-- Published answers are never touched (spec §2 lock).

-- name: RecomputeOfficials :execrows
UPDATE answers a
SET official_record_id = pick.rec_id,
    official_set_by = NULL,
    official_set_at = CASE WHEN pick.rec_id IS NULL THEN NULL ELSE now() END,
    updated_at = now()
FROM (
    SELECT ans.id AS answer_id,
        COALESCE(CASE WHEN cardinality(ans.flags) = 0 THEN src.id END, hum.id) AS rec_id
    FROM answers ans
    LEFT JOIN LATERAL (
        SELECT gr.id
        FROM grading_records gr
        WHERE gr.answer_id = ans.id
          AND gr.total IS NOT NULL
          AND gr.rubric_version_id = (
              SELECT rv.id FROM rubric_versions rv
              WHERE rv.problem_id = ans.problem_id
              ORDER BY rv.version DESC
              LIMIT 1)
          AND ((sqlc.narg(final_run_id)::bigint IS NOT NULL
                  AND gr.source = 'model'
                  AND gr.run_id = sqlc.narg(final_run_id))
              OR (sqlc.narg(final_run_id)::bigint IS NULL
                  AND gr.source = 'aggregate'))
        ORDER BY gr.id DESC
        LIMIT 1
    ) src ON true
    LEFT JOIN LATERAL (
        SELECT gr.id
        FROM grading_records gr
        WHERE gr.answer_id = ans.id
          AND gr.source = 'human'
          AND gr.total IS NOT NULL
          AND gr.rubric_version_id = (
              SELECT rv.id FROM rubric_versions rv
              WHERE rv.problem_id = ans.problem_id
              ORDER BY rv.version DESC
              LIMIT 1)
        ORDER BY gr.id DESC
        LIMIT 1
    ) hum ON true
    WHERE ans.assessment_id = $1 AND ans.published_at IS NULL
) pick
WHERE a.id = pick.answer_id
  AND a.official_record_id IS DISTINCT FROM pick.rec_id;

-- ClearUnpublishedOfficials is RecomputeOfficials' "no source chosen" arm:
-- before an exam picks its final source, nothing is official.
-- name: ClearUnpublishedOfficials :execrows
UPDATE answers
SET official_record_id = NULL,
    official_set_by = NULL,
    official_set_at = NULL,
    updated_at = now()
WHERE assessment_id = $1
  AND published_at IS NULL
  AND official_record_id IS NOT NULL;
