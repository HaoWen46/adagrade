-- Grading-policy pin on records (DECISIONS D25): each model record captures the
-- judgment stance (lenient/standard/strict) it was graded under, alongside the pinned
-- prompt-template version — together they make the judgment language reproducible.
-- NULL means the stance does not apply: human, aggregate, and legacy records.

-- +goose Up
ALTER TABLE grading_records
    ADD COLUMN policy TEXT CHECK (policy IS NULL OR policy IN ('lenient', 'standard', 'strict'));

-- +goose Down
ALTER TABLE grading_records DROP COLUMN policy;
