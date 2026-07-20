-- +goose Up
-- Parked-page conflicts record the CONTESTED cell at park time (intake-races
-- fix, 2026-07-11). ResolveConflict's "replace" used to copy the incumbent's
-- CURRENT assignment at resolve time: if the incumbent had meanwhile been
-- reassigned elsewhere, replace evicted it from its new (correct) cell — and
-- if it had been unassigned, replace wrote a NULL-cell assignment. Capturing
-- the cell where the fight actually happened keeps "replace" meaning "put the
-- parked page where the fight happened" no matter what the incumbent did
-- since. Rows parked before this migration carry NULL/NULL; ResolveConflict
-- falls back to the incumbent's current cell for those.
ALTER TABLE scan_pages
    ADD COLUMN park_student_id BIGINT REFERENCES students (id),
    ADD COLUMN park_problem_id BIGINT;
ALTER TABLE scan_pages
    ADD CONSTRAINT scan_pages_park_problem_assessment_fkey
        FOREIGN KEY (park_problem_id, assessment_id) REFERENCES problems (id, assessment_id);
-- the contested cell is a CELL: both set or both null (mirrors the
-- assigned_student_id/assigned_problem_id check in 0029)
ALTER TABLE scan_pages
    ADD CONSTRAINT scan_pages_park_cell_chk
        CHECK ((park_student_id IS NULL) = (park_problem_id IS NULL));

-- +goose Down
ALTER TABLE scan_pages DROP CONSTRAINT scan_pages_park_cell_chk;
ALTER TABLE scan_pages DROP CONSTRAINT scan_pages_park_problem_assessment_fkey;
ALTER TABLE scan_pages DROP COLUMN park_student_id;
ALTER TABLE scan_pages DROP COLUMN park_problem_id;
