-- +goose Up
-- Calibration sample runs (PDF-aligned simplification, spec
-- 2026-07-20-pdf-aligned-simplification-design.md §1): a fourth run scope
-- 'sample' whose scope_id carries the requested sample size N. The concrete
-- answer set is drawn deterministically at plan time (problem-stratified,
-- seeded by run id — grading.SelectCalibrationSample) and persisted as ordinary
-- run items, so the sample is recorded and reproducible. Replaces the guide's
-- "launch one answer-scoped run per calibration answer" workflow.

ALTER TABLE grading_runs
    DROP CONSTRAINT grading_runs_scope_kind_check;
ALTER TABLE grading_runs
    ADD CONSTRAINT grading_runs_scope_kind_check
        CHECK (scope_kind IN ('answer', 'problem', 'assessment', 'sample'));

-- +goose Down
-- Sample-scope rows violate the narrower check and retagging them is wrong
-- (scope_id is N, not an answer id), so delete them — they are calibration
-- probes, never a final source (final pins are assessment-scope only) and
-- never the official-record source (officials join on final_run_id).
-- grading_records.run_id does NOT cascade, so remove referrers first:
-- run_items (blocks record deletes via record_id), spot_checks (blocks via
-- grading_record_id), then the records, then the runs (waived_runs cascades).
DELETE FROM grading_run_items
    WHERE run_id IN (SELECT id FROM grading_runs WHERE scope_kind = 'sample');
DELETE FROM spot_checks
    WHERE run_id IN (SELECT id FROM grading_runs WHERE scope_kind = 'sample');
DELETE FROM grading_records
    WHERE run_id IN (SELECT id FROM grading_runs WHERE scope_kind = 'sample');
DELETE FROM grading_runs WHERE scope_kind = 'sample';
-- assessments_final_run_fk (migration 0035) is DEFERRABLE INITIALLY DEFERRED,
-- so the DELETE above queues per-row RI trigger events that would otherwise
-- make the ALTER TABLE below fail with "pending trigger events" (SQLSTATE
-- 55006) whenever any sample-scope row existed. Force the checks to run now.
SET CONSTRAINTS ALL IMMEDIATE;
ALTER TABLE grading_runs
    DROP CONSTRAINT grading_runs_scope_kind_check;
ALTER TABLE grading_runs
    ADD CONSTRAINT grading_runs_scope_kind_check
        CHECK (scope_kind IN ('answer', 'problem', 'assessment'));
