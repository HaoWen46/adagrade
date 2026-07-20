-- Submissions, answers (natural key, pre-materialized), pages, masking (D1/D2/D10).

-- +goose Up
CREATE TABLE submissions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    student_id BIGINT NOT NULL REFERENCES students (id),
    original_filename TEXT NOT NULL,
    pdf_ref TEXT NOT NULL,       -- BlobStore key of the untouched source PDF
    pdf_sha256 TEXT NOT NULL,
    page_count INT NOT NULL,
    uploaded_by BIGINT REFERENCES users (id),
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by BIGINT REFERENCES submissions (id) -- re-upload chain (D1)
);
-- One live submission per (assessment, student); older ones are superseded, never deleted.
CREATE UNIQUE INDEX submissions_active_uniq ON submissions (assessment_id, student_id)
    WHERE superseded_by IS NULL;

-- The atomic gradable unit, keyed by its natural identity (D1). Status is derived,
-- never stored (D2) — these columns are facts, not lifecycle state.
CREATE TABLE answers (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    student_id BIGINT NOT NULL REFERENCES students (id),
    problem_id BIGINT NOT NULL,
    official_record_id BIGINT,   -- FK added in 0004 (grading_records not yet created)
    official_set_by BIGINT REFERENCES users (id),
    official_set_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,    -- set by Phase 6 publish; guards re-pointing (D1/D6)
    flags TEXT [] NOT NULL DEFAULT '{}', -- e.g. image_superseded, illegible, mask_review, manual
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (assessment_id, student_id, problem_id),
    -- Composite FK: an answer's problem must belong to the answer's assessment.
    FOREIGN KEY (problem_id, assessment_id) REFERENCES problems (id, assessment_id) ON DELETE CASCADE
);

-- Multi-page answers are first-class (D1). Pages reference the source PDF page
-- directly; no split PDFs exist. mask_review_* is the per-page QA gate (D10).
CREATE TABLE answer_pages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    answer_id BIGINT NOT NULL REFERENCES answers (id) ON DELETE CASCADE,
    page_index INT NOT NULL,     -- order within the answer
    submission_id BIGINT NOT NULL REFERENCES submissions (id),
    pdf_page_index INT NOT NULL, -- 0-based page in the submission PDF
    image_ref TEXT NOT NULL,     -- rendered JPG (humans see this)
    image_sha256 TEXT NOT NULL,  -- provenance: records store the SHAs they graded
    image_width INT NOT NULL,
    image_height INT NOT NULL,
    masked_image_ref TEXT,       -- derived artifact; ONLY this ever reaches a provider
    masked_at TIMESTAMPTZ,
    mask_review_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (mask_review_status IN ('pending', 'accepted', 'flagged')),
    mask_reviewed_by BIGINT REFERENCES users (id),
    mask_reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (answer_id, page_index)
);
CREATE INDEX answer_pages_submission_idx ON answer_pages (submission_id);

-- Normalized (0..1) redaction rects per assessment (spec §7).
CREATE TABLE mask_regions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    page_scope TEXT NOT NULL DEFAULT 'first' CHECK (page_scope IN ('first', 'all')),
    x REAL NOT NULL CHECK (x >= 0 AND x <= 1),
    y REAL NOT NULL CHECK (y >= 0 AND y <= 1),
    w REAL NOT NULL CHECK (w > 0 AND w <= 1),
    h REAL NOT NULL CHECK (h > 0 AND h <= 1),
    color TEXT NOT NULL DEFAULT '#4a4a4a', -- not pure black: some models over-attend to it
    padding REAL NOT NULL DEFAULT 0.01,    -- absorbs scan drift
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mask_regions_assessment_idx ON mask_regions (assessment_id);

-- +goose Down
DROP TABLE mask_regions;
DROP TABLE answer_pages;
DROP TABLE answers;
DROP TABLE submissions;
