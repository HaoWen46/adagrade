-- Async batch operations (D27; audit findings F1/F2/F16). Finalize promotion,
-- ApplyMasks, and bulk direct upload cannot complete synchronously in one HTTP
-- request at 200×9 scale — they move onto River workers. This migration adds the
-- schema those workers need:
--
--   * answer_pages.mask_input_sha — a fingerprint of the masking INPUTS (original
--     image sha + quality + the canonical region set) recorded when the masked
--     artifact is written. A per-page re-apply job compares this against the
--     freshly-computed fingerprint to skip pages already up to date (preserving
--     their review status) instead of blindly re-masking + resetting all ~1800
--     pages every click (F2).
--
--   * direct_uploads — one staged bulk direct-upload file awaiting the ingest
--     worker (F1's sibling for the direct-upload path). status/reason/submission_id
--     mirror ingest.FileResult; NULL finished_at means the job has not run yet.

-- +goose Up
ALTER TABLE answer_pages ADD COLUMN mask_input_sha TEXT;

CREATE TABLE direct_uploads (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_kind TEXT NOT NULL DEFAULT 'pdf' CHECK (source_kind IN ('pdf', 'image')),
    force BOOL NOT NULL DEFAULT FALSE,
    uploaded_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,          -- NULL = pending (job not yet finished)
    status TEXT CHECK (status IN ('ingested', 'quarantined', 'rejected')),
    reason TEXT,
    submission_id BIGINT REFERENCES submissions (id),
    error TEXT
);
CREATE INDEX direct_uploads_assessment_idx ON direct_uploads (assessment_id, id DESC);

-- +goose Down
DROP TABLE direct_uploads;
ALTER TABLE answer_pages DROP COLUMN mask_input_sha;
