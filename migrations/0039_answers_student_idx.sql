-- answers.student_id is the one child-side FK 0013 skipped. Harmless while every
-- query was per-assessment — UNIQUE (assessment_id, student_id, problem_id)
-- covers those — but the per-student page (students_page.sql, 2026-07-28) filters
-- on student_id with no assessment predicate, and the roster hard-delete guard
-- (GetStudentBlockingArtifacts) has the same shape: without this index both
-- seq-scan the whole answers table (roster × problems × assessments) per lookup.

-- +goose Up
CREATE INDEX answers_student_idx ON answers (student_id);

-- +goose Down
DROP INDEX answers_student_idx;
