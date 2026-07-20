-- Post-hoc multi-model consensus (DECISIONS D17): per-assessment policy + a third
-- record source 'aggregate' for combined results.

-- +goose Up
CREATE TABLE aggregation_policies (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL UNIQUE REFERENCES assessments (id) ON DELETE CASCADE,
    method_version_ids BIGINT [] NOT NULL,
    combiner TEXT NOT NULL DEFAULT 'majority' CHECK (combiner IN ('majority', 'mean')),
    fault_tolerance INT NOT NULL DEFAULT 0 CHECK (fault_tolerance >= 0),
    flag_triggers TEXT [] NOT NULL DEFAULT '{agg_disagreement,agg_missing,agg_low_confidence}',
    set_official BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by BIGINT REFERENCES users (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE grading_records DROP CONSTRAINT grading_records_source_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_source_check
    CHECK (source IN ('model', 'human', 'aggregate'));
-- Aggregates record who ran them, like human records do.
ALTER TABLE grading_records DROP CONSTRAINT grading_records_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_check
    CHECK (source = 'model' OR created_by IS NOT NULL);

-- +goose Down
ALTER TABLE grading_records DROP CONSTRAINT grading_records_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_check
    CHECK (source <> 'human' OR created_by IS NOT NULL);
DELETE FROM grading_records WHERE source = 'aggregate';
ALTER TABLE grading_records DROP CONSTRAINT grading_records_source_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_source_check
    CHECK (source IN ('model', 'human'));
DROP TABLE aggregation_policies;
