-- +goose Up
-- Round-based grading (2026-07-04 design decision): the exam — not the answer —
-- is the unit of decision. Each assessment picks ONE final grading source
-- (a single method, or the consensus); answers.official_record_id becomes a
-- DERIVED pointer (source record where decided, latest human record as
-- fallback) recomputed by store.RecomputeOfficials instead of being set
-- per-answer by hand. Regrades become sparse per-turn overlay rounds, each
-- with its own method, gated by a per-exam deadline.

ALTER TABLE assessments
    ADD COLUMN final_source_kind TEXT
        CHECK (final_source_kind IN ('method', 'consensus')),
    ADD COLUMN final_method_id BIGINT REFERENCES grading_methods (id),
    ADD COLUMN regrade_deadline TIMESTAMPTZ;

-- kind='method' must carry the method id; kind='consensus' (or unset) must not.
ALTER TABLE assessments
    ADD CONSTRAINT assessments_final_source_check
        CHECK ((final_source_kind = 'method') = (final_method_id IS NOT NULL));

-- One grading method per regrade round (email turn); preset on the Regrade tab,
-- editable until the round has graded requests. Usually a single strict model —
-- a consensus here would just manufacture new conflicts for humans to resolve.
CREATE TABLE regrade_round_methods (
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    turn INT NOT NULL CHECK (turn >= 1),
    method_id BIGINT NOT NULL REFERENCES grading_methods (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (assessment_id, turn)
);

-- +goose Down
DROP TABLE regrade_round_methods;
ALTER TABLE assessments
    DROP CONSTRAINT assessments_final_source_check,
    DROP COLUMN regrade_deadline,
    DROP COLUMN final_method_id,
    DROP COLUMN final_source_kind;
