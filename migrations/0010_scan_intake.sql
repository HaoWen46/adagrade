-- Scan intake & student identification staging pipeline (D18-D23); see design spec
-- docs/superpowers/specs/2026-07-02-scan-intake-identification-design.md §3.

-- +goose Up

-- Rename to source_* so submissions can carry either a PDF or a single image (D22).
ALTER TABLE submissions RENAME COLUMN pdf_ref TO source_ref;
ALTER TABLE submissions RENAME COLUMN pdf_sha256 TO source_sha256;

ALTER TABLE submissions
    ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'pdf' CHECK (source_kind IN ('pdf', 'image')),
    ADD COLUMN problem_id BIGINT NULL,
    ADD COLUMN retracted_at TIMESTAMPTZ;

-- Composite FK: a per-problem image submission's problem must belong to its
-- assessment (mirrors the answers table pattern in 0003).
ALTER TABLE submissions
    ADD CONSTRAINT submissions_problem_assessment_fkey
        FOREIGN KEY (problem_id, assessment_id) REFERENCES problems (id, assessment_id);

-- Replace the single active-unique index with two partial indexes: whole-assessment
-- submissions (problem_id IS NULL) and per-problem image submissions (D22),
-- both scoped to live (not superseded, not retracted) rows (§7 unassignment).
DROP INDEX submissions_active_uniq;
CREATE UNIQUE INDEX submissions_active_whole_uniq ON submissions (assessment_id, student_id)
    WHERE superseded_by IS NULL AND retracted_at IS NULL AND problem_id IS NULL;
CREATE UNIQUE INDEX submissions_active_problem_uniq ON submissions (assessment_id, student_id, problem_id)
    WHERE superseded_by IS NULL AND retracted_at IS NULL AND problem_id IS NOT NULL;

-- Withdrawn students keep their historical records but are excluded from active
-- rosters/matching going forward (D23).
ALTER TABLE students ADD COLUMN withdrawn_at TIMESTAMPTZ;

-- A scan batch is one upload (zip of scans, or a run of individual files) awaiting
-- identification. problem_id set ⇒ every file in the batch is a single-page,
-- per-problem image submission (D22); NULL ⇒ whole-assessment PDF submissions.
CREATE TABLE scan_batches (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    problem_id BIGINT NULL,
    ocr_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ocr_provider TEXT,
    ocr_model TEXT,
    zip_ref TEXT,
    finalized_at TIMESTAMPTZ,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (problem_id, assessment_id) REFERENCES problems (id, assessment_id)
);
CREATE INDEX scan_batches_assessment_idx ON scan_batches (assessment_id);

-- One staged scan file (one page-0-derived identity crop, one candidate student).
-- Status is derived, never stored (D2): error → discarded → promoted → assigned →
-- proposed → unidentified → processing.
CREATE TABLE scan_files (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (source_kind IN ('pdf', 'image')),
    page_count INT,
    page0_image_ref TEXT,
    page0_width INT,
    page0_height INT,
    id_crop_ref TEXT,
    ocr_student_id TEXT,
    ocr_name TEXT,
    ocr_legible BOOLEAN,
    proposed_student_id BIGINT REFERENCES students (id),
    proposal_source TEXT CHECK (proposal_source IN ('filename', 'ocr_id', 'ocr_fuzzy', 'ocr_name')),
    assigned_student_id BIGINT REFERENCES students (id),
    assigned_by BIGINT REFERENCES users (id),
    assigned_at TIMESTAMPTZ,
    discarded_at TIMESTAMPTZ,
    discard_reason TEXT,
    submission_id BIGINT REFERENCES submissions (id),
    error TEXT,
    attempts INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, source_sha256)
);
CREATE INDEX scan_files_batch_idx ON scan_files (batch_id);

-- Normalized (0..1) ID-box rects, mirroring mask_regions (§7) but scoped to a single
-- source page (page_index) instead of page_scope — all rects for an assessment must
-- share one page_index (validated on PUT, one crop source page).
CREATE TABLE id_regions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    page_index INT NOT NULL DEFAULT 0,
    x REAL NOT NULL CHECK (x >= 0 AND x <= 1),
    y REAL NOT NULL CHECK (y >= 0 AND y <= 1),
    w REAL NOT NULL CHECK (w > 0 AND w <= 1),
    h REAL NOT NULL CHECK (h > 0 AND h <= 1),
    color TEXT NOT NULL DEFAULT '#4a4a4a',
    padding REAL NOT NULL DEFAULT 0.01,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX id_regions_assessment_idx ON id_regions (assessment_id);

-- +goose Down
DROP TABLE id_regions;
DROP TABLE scan_files;
DROP TABLE scan_batches;

ALTER TABLE students DROP COLUMN withdrawn_at;

DROP INDEX submissions_active_problem_uniq;
DROP INDEX submissions_active_whole_uniq;
CREATE UNIQUE INDEX submissions_active_uniq ON submissions (assessment_id, student_id)
    WHERE superseded_by IS NULL;

ALTER TABLE submissions DROP CONSTRAINT submissions_problem_assessment_fkey;
ALTER TABLE submissions
    DROP COLUMN retracted_at,
    DROP COLUMN problem_id,
    DROP COLUMN source_kind;

ALTER TABLE submissions RENAME COLUMN source_sha256 TO pdf_sha256;
ALTER TABLE submissions RENAME COLUMN source_ref TO pdf_ref;
