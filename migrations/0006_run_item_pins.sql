-- A grading method spans problems, but rubric/reference-solution versions are
-- per-problem — so the RUN PLANNER resolves and pins the concrete versions per leaf
-- at plan time (D5/B-H18: a mid-run rubric edit cannot leak into a running run).

-- +goose Up
ALTER TABLE grading_run_items
    ADD COLUMN rubric_version_id BIGINT REFERENCES rubric_versions (id),
    ADD COLUMN reference_solution_version_id BIGINT REFERENCES reference_solution_versions (id);

-- +goose Down
ALTER TABLE grading_run_items
    DROP COLUMN reference_solution_version_id,
    DROP COLUMN rubric_version_id;
