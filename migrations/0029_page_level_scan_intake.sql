-- +goose Up
-- Page-level scan intake (design spec 2026-07-04): the staging unit becomes one
-- physical PAGE, assigned to a (student, problem) cell. Replaces the file-level
-- scan_files flow (one file = one student's whole paper, positional page→problem
-- mapping). DEPLOY NOTE: any batch still staged in scan_files is dropped here —
-- finalize or discard in-flight batches before upgrading.

DROP TABLE scan_files;

-- A batch is just "one uploaded scanner run": per-problem scoping is obsolete
-- (every page names its own problem) and batch-level finalized_at is meaningless
-- (finalize is assessment-wide and incremental). source_ref holds the zip blob
-- when the batch was a zip upload.
ALTER TABLE scan_batches DROP COLUMN problem_id;
ALTER TABLE scan_batches DROP COLUMN finalized_at;
ALTER TABLE scan_batches RENAME COLUMN zip_ref TO source_ref;

-- id_regions become typed: exactly one live region per kind, applied to EVERY
-- page (the old single-identity-page page_index is gone). Existing rows carry no
-- kind, so they are dropped — regions must be redrawn once after this upgrade.
DELETE FROM id_regions;
ALTER TABLE id_regions DROP COLUMN page_index;
ALTER TABLE id_regions
    ADD COLUMN kind TEXT NOT NULL CHECK (kind IN ('student_id', 'name', 'problem_id'));

-- One uploaded source file (a scanner PDF or a loose image). A batch may carry
-- several sources (several PDFs, or a zip's entries), so page idempotency is
-- per-source, not per-batch.
CREATE TABLE scan_sources (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (source_kind IN ('pdf', 'image')),
    page_count INT,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, source_sha256)
);
CREATE INDEX scan_sources_batch_idx ON scan_sources (batch_id);

-- One physical page. OCR text columns are PII (D14): DB-bound rows and
-- staff-facing JSON only, never logs or job args. Status is derived, never
-- stored (D2): error → discarded → promoted → parked → assigned → orphan
-- (identified_at set, no assignment) → processing.
CREATE TABLE scan_pages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT NOT NULL REFERENCES scan_sources (id) ON DELETE CASCADE,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    page_index INT NOT NULL,
    image_ref TEXT,
    image_sha256 TEXT,
    image_width INT,
    image_height INT,
    student_id_crop_ref TEXT,
    name_crop_ref TEXT,
    problem_crop_ref TEXT,
    ocr_student_id TEXT,
    ocr_name TEXT,
    ocr_problem TEXT,
    ocr_student_id_legible BOOLEAN,
    ocr_name_legible BOOLEAN,
    ocr_problem_legible BOOLEAN,
    ocr_engine TEXT,
    identified_at TIMESTAMPTZ,
    proposed_student_id BIGINT REFERENCES students (id),
    proposed_problem_id BIGINT,
    proposal_source TEXT CHECK (proposal_source IN ('ocr_agree', 'ocr_id', 'ocr_name', 'ocr_disagree')),
    assigned_student_id BIGINT REFERENCES students (id),
    assigned_problem_id BIGINT,
    assigned_by BIGINT REFERENCES users (id),
    assigned_at TIMESTAMPTZ,
    force_promote BOOLEAN NOT NULL DEFAULT FALSE,
    parked_reason TEXT CHECK (parked_reason IN ('duplicate', 'conflict')),
    parked_against BIGINT REFERENCES scan_pages (id),
    discarded_at TIMESTAMPTZ,
    discard_reason TEXT,
    submission_id BIGINT REFERENCES submissions (id),
    error TEXT,
    attempts INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- split idempotency under River redelivery
    UNIQUE (source_id, page_index),
    -- assignment is to a CELL: both set or both null
    CHECK ((assigned_student_id IS NULL) = (assigned_problem_id IS NULL)),
    -- a proposed/assigned problem must belong to this page's assessment
    -- (mirrors the answers pattern in 0003)
    FOREIGN KEY (proposed_problem_id, assessment_id) REFERENCES problems (id, assessment_id),
    FOREIGN KEY (assigned_problem_id, assessment_id) REFERENCES problems (id, assessment_id)
);
CREATE INDEX scan_pages_batch_idx ON scan_pages (batch_id);
CREATE INDEX scan_pages_assessment_idx ON scan_pages (assessment_id);
CREATE INDEX scan_pages_source_idx ON scan_pages (source_id);
-- One live assigned page per (assessment, student, problem) cell — mirrors
-- submissions_active_problem_uniq.
CREATE UNIQUE INDEX scan_pages_live_cell_uniq
    ON scan_pages (assessment_id, assigned_student_id, assigned_problem_id)
    WHERE assigned_student_id IS NOT NULL AND discarded_at IS NULL;

-- +goose Down
DROP TABLE scan_pages;
DROP TABLE scan_sources;

ALTER TABLE id_regions DROP COLUMN kind;
ALTER TABLE id_regions ADD COLUMN page_index INT NOT NULL DEFAULT 0;

ALTER TABLE scan_batches RENAME COLUMN source_ref TO zip_ref;
ALTER TABLE scan_batches ADD COLUMN finalized_at TIMESTAMPTZ;
ALTER TABLE scan_batches ADD COLUMN problem_id BIGINT NULL;
ALTER TABLE scan_batches
    ADD CONSTRAINT scan_batches_problem_assessment_fkey
        FOREIGN KEY (problem_id, assessment_id) REFERENCES problems (id, assessment_id);

-- scan_files restored per 0010 + ocr_engine from 0011.
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
    ocr_engine TEXT,
    UNIQUE (batch_id, source_sha256)
);
-- NOTE: no CREATE INDEX scan_files_batch_idx here — migration 0013's Down
-- recreates that index later in the same down-to-0 walk (0013 DROPped it as
-- redundant with this UNIQUE(batch_id, source_sha256)); creating it here too
-- would collide when 0013's Down runs (relation already exists, SQLSTATE 42P07).
