-- Model pricing + spend queries (trust spec §2-3).

-- name: UpsertModelPricing :one
INSERT INTO model_pricing (provider_id, model, input_usd_per_mtok, output_usd_per_mtok)
VALUES ($1, $2, $3, $4)
ON CONFLICT (provider_id, model) DO UPDATE
    SET input_usd_per_mtok = EXCLUDED.input_usd_per_mtok,
        output_usd_per_mtok = EXCLUDED.output_usd_per_mtok,
        updated_at = now()
RETURNING *;

-- name: ListModelPricing :many
SELECT * FROM model_pricing WHERE provider_id = $1 ORDER BY model;

-- name: GetModelPricing :one
SELECT * FROM model_pricing WHERE provider_id = $1 AND model = $2;

-- name: ListAllModelPricing :many
SELECT * FROM model_pricing ORDER BY provider_id, model;

-- MonthToDateCost sums cost_usd for grading_records created since the start of the
-- current UTC month (D36 monthly global cap). NULL cost_usd rows (no pricing at
-- insert time) contribute 0, matching "absence is visible, not a fake charge" --
-- the caller separately surfaces missing-pricing coverage via ListModelPricing.
-- Both run-leaf ('model') AND stricter-AI-regrade ('regrade_ai', migration 0024,
-- spec §8) records are real provider spend and must both count toward month-to-date —
-- a regrade_ai record's cost is otherwise invisible to the budget gate.
-- name: MonthToDateCost :one
SELECT COALESCE(sum(cost_usd), 0)::numeric AS total
FROM grading_records
WHERE source IN ('model', 'regrade_ai')
  AND created_at >= (date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC');

-- name: RunCost :one
SELECT COALESCE(sum(cost_usd), 0)::numeric AS total,
    COALESCE(sum(input_tokens), 0)::bigint AS input_tokens,
    COALESCE(sum(output_tokens), 0)::bigint AS output_tokens
FROM grading_records
WHERE run_id = $1;

-- name: SetRunCostCap :one
UPDATE grading_runs SET cost_cap_usd = $2 WHERE id = $1 RETURNING *;
