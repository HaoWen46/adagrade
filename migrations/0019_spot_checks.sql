-- Spot-check gate (trust spec §4, §8; closes PLAN_GAPS B-C5): a run's grades cannot be
-- bulk-accepted as official until a human has spot-checked a stratified sample.
--
-- spot_checks: one row per sampled grading_record, verdict recorded by a checker.
-- waived_runs: admin override (POST /api/runs/{id}/spot-check/waive, D37) OR the
-- migration backfill below — pre-existing completed runs have no samples, so without
-- a waiver this feature would retroactively lock grading history that was already
-- reviewed under the old process. Marked with reason='migration' so it's distinguishable
-- from a deliberate admin override in the audit trail.

-- +goose Up
CREATE TABLE spot_checks (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES grading_runs (id) ON DELETE CASCADE,
    grading_record_id BIGINT NOT NULL REFERENCES grading_records (id),
    verdict TEXT CHECK (verdict IS NULL OR verdict IN ('agree', 'adjusted')), -- NULL = not yet checked
    note TEXT NOT NULL DEFAULT '',
    checker_id BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    checked_at TIMESTAMPTZ,
    UNIQUE (run_id, grading_record_id)
);
CREATE INDEX spot_checks_run_idx ON spot_checks (run_id);

CREATE TABLE waived_runs (
    run_id BIGINT PRIMARY KEY REFERENCES grading_runs (id) ON DELETE CASCADE,
    reason TEXT NOT NULL, -- 'migration' (backfill below) or an admin-supplied reason
    waived_by BIGINT REFERENCES users (id), -- NULL for the migration backfill
    waived_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill: runs that were already 'completed' before this feature shipped get no
-- retroactive gate. Runs still pending/running/etc. are ungated until they complete and
-- get sampled normally by the app. Note the gate's actual polarity (runs.go
-- spotCheckGate.open): total=0 && !waived is CLOSED — open() is
-- `waived || (total>0 && done==total)`, so an un-waived run with no sample does NOT
-- open. The backfill exists precisely because of that: it waives pre-existing completed
-- runs so their (empty-sample) gate is open, keeping grading history usable. Only runs
-- that actually produced graded leaves need this waiver.
INSERT INTO waived_runs (run_id, reason, waived_by, waived_at)
SELECT id, 'migration', NULL, now()
FROM grading_runs
WHERE status = 'completed';

-- +goose Down
DROP TABLE waived_runs;
DROP TABLE spot_checks;
