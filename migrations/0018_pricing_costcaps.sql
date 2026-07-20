-- Model pricing + budget caps (trust spec §2-3, §8; closes PLAN_GAPS B-H5).
--
--   model_pricing    operator-entered $/Mtok in/out per (provider, model); cost_usd is
--                    computed at record-insert time from token counts × this table
--                    (NULL pricing ⇒ NULL cost_usd — absence stays visible, never a
--                    fake zero). No historical backfill: a pricing edit affects only
--                    future records (flagged in the design doc).
--   runs.cost_cap_usd   nullable per-run spend cap (D36); the leaf executor checks
--                    accumulated SUM(cost_usd) before each grade call.

-- +goose Up
CREATE TABLE model_pricing (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES llm_providers (id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    input_usd_per_mtok NUMERIC(10, 4) NOT NULL CHECK (input_usd_per_mtok >= 0),
    output_usd_per_mtok NUMERIC(10, 4) NOT NULL CHECK (output_usd_per_mtok >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, model)
);

ALTER TABLE grading_runs ADD COLUMN cost_cap_usd NUMERIC(10, 2) CHECK (cost_cap_usd IS NULL OR cost_cap_usd >= 0);

-- +goose Down
ALTER TABLE grading_runs DROP COLUMN cost_cap_usd;
DROP TABLE model_pricing;
