-- Unindexed FKs make delete cascades (and any FK-check probe) seq-scan the
-- referencing children — verified via live EXPLAIN against grading_run_items,
-- answers, and grading_records (audit finding). Add the missing child-side
-- indexes, and drop scan_files_batch_idx (0010), which is fully redundant with
-- the UNIQUE(batch_id, source_sha256) index already covering batch_id as its
-- leading column.

-- +goose Up
CREATE INDEX grading_run_items_answer_idx ON grading_run_items (answer_id);
CREATE INDEX grading_run_items_record_idx ON grading_run_items (record_id);
CREATE INDEX answers_official_record_idx ON answers (official_record_id) WHERE official_record_id IS NOT NULL;
CREATE INDEX grading_records_rubric_version_idx ON grading_records (rubric_version_id);
DROP INDEX scan_files_batch_idx; -- fully redundant with UNIQUE(batch_id, source_sha256)

-- +goose Down
CREATE INDEX scan_files_batch_idx ON scan_files (batch_id);
DROP INDEX grading_records_rubric_version_idx;
DROP INDEX answers_official_record_idx;
DROP INDEX grading_run_items_record_idx;
DROP INDEX grading_run_items_answer_idx;
