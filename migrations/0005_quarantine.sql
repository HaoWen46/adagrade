-- Quarantine for uploads that cannot be auto-matched to the roster (D13): the file
-- is stored, surfaced in the mapping review, and never silently dropped.

-- +goose Up
CREATE TABLE upload_quarantine (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    pdf_ref TEXT NOT NULL,
    pdf_sha256 TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('unknown_student', 'invalid_pdf', 'duplicate_in_batch')),
    uploaded_by BIGINT REFERENCES users (id),
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resolved_student_id BIGINT REFERENCES students (id)
);
CREATE INDEX upload_quarantine_assessment_idx ON upload_quarantine (assessment_id) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE upload_quarantine;
