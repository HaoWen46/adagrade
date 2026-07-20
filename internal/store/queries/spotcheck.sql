-- Spot-check gate (trust spec §4).

-- name: InsertSpotCheck :one
INSERT INTO spot_checks (run_id, grading_record_id)
VALUES ($1, $2)
ON CONFLICT (run_id, grading_record_id) DO NOTHING
RETURNING *;

-- name: ListSpotChecks :many
SELECT sc.*, gr.answer_id, gr.total, gr.criterion_scores, gr.confidence,
    p.number AS problem_number
FROM spot_checks sc
JOIN grading_records gr ON gr.id = sc.grading_record_id
JOIN answers a ON a.id = gr.answer_id
JOIN problems p ON p.id = a.problem_id
WHERE sc.run_id = $1
ORDER BY p.number, sc.id;

-- name: GetSpotCheck :one
SELECT * FROM spot_checks WHERE id = $1;

-- name: SetSpotCheckVerdict :one
UPDATE spot_checks
SET verdict = $2, note = $3, checker_id = $4, checked_at = now()
WHERE id = $1
RETURNING *;

-- name: SpotCheckCounts :one
SELECT count(*) AS total, count(*) FILTER (WHERE verdict IS NOT NULL) AS done
FROM spot_checks
WHERE run_id = $1;

-- name: IsRunWaived :one
SELECT EXISTS (SELECT 1 FROM waived_runs WHERE run_id = $1);

-- name: WaiveSpotCheck :exec
INSERT INTO waived_runs (run_id, reason, waived_by)
VALUES ($1, $2, $3)
ON CONFLICT (run_id) DO UPDATE SET reason = EXCLUDED.reason, waived_by = EXCLUDED.waived_by, waived_at = now();

-- name: SpotCheckAgreementRate :one
SELECT count(*) FILTER (WHERE verdict = 'agree') AS agreed, count(*) AS total
FROM spot_checks
WHERE run_id = $1 AND verdict IS NOT NULL;
