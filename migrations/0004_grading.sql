-- Grading methods (config-as-data), runs, per-leaf items, append-only records (spec §5-6; D5/D6/D12).

-- +goose Up
CREATE TABLE prompt_template_versions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL,
    version INT NOT NULL,
    system_template TEXT NOT NULL,
    user_template TEXT NOT NULL,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

CREATE TABLE grading_methods (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Method config is data (plan §4): provider, model, temperature (default 0, D-H2),
-- reasoning level, #reference solutions, re-ask cap, pinned rubric/prompt/refsol
-- versions (D5). Editing creates version N+1; runs snapshot a version at launch.
CREATE TABLE grading_method_versions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    method_id BIGINT NOT NULL REFERENCES grading_methods (id) ON DELETE CASCADE,
    version INT NOT NULL,
    config JSONB NOT NULL,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (method_id, version)
);

CREATE TABLE grading_runs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('answer', 'problem', 'assessment')),
    scope_id BIGINT NOT NULL,
    method_version_id BIGINT NOT NULL REFERENCES grading_method_versions (id),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'paused', 'cancelled', 'completed', 'failed')),
    execution_mode TEXT NOT NULL DEFAULT 'sync' CHECK (execution_mode IN ('sync', 'batch')),
    error TEXT,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX grading_runs_assessment_idx ON grading_runs (assessment_id);

-- Append-only, immutable results — one per (answer, run, model) or one per manual
-- grade. total is app-computed from clamped/snapped criterion scores (D4); NULL total
-- means the model took the explicit illegible/uncertain refusal path (D12).
CREATE TABLE grading_records (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    answer_id BIGINT NOT NULL REFERENCES answers (id) ON DELETE CASCADE,
    run_id BIGINT REFERENCES grading_runs (id), -- NULL for manual/human grades
    source TEXT NOT NULL CHECK (source IN ('model', 'human')),
    provider TEXT,
    model_id TEXT,
    method_version_id BIGINT REFERENCES grading_method_versions (id),
    rubric_version_id BIGINT NOT NULL REFERENCES rubric_versions (id),
    reference_solution_version_id BIGINT REFERENCES reference_solution_versions (id),
    prompt_template_version_id BIGINT REFERENCES prompt_template_versions (id),
    graded_image_shas TEXT [] NOT NULL DEFAULT '{}', -- image provenance (D1)
    criterion_scores JSONB NOT NULL,  -- [{criterion_id, score, rationale}]
    total NUMERIC(6, 2),
    comment TEXT NOT NULL DEFAULT '',
    transcription TEXT,
    confidence TEXT CHECK (confidence IN ('high', 'medium', 'low', 'illegible')),
    adjustments JSONB NOT NULL DEFAULT '[]'::jsonb, -- clamp/snap corrections applied in Go (D4)
    raw_output JSONB,
    input_tokens INT,
    output_tokens INT,
    cost_usd NUMERIC(10, 6),
    temperature REAL,
    created_by BIGINT REFERENCES users (id), -- the human, for source='human'
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source <> 'human' OR created_by IS NOT NULL),
    CHECK (source <> 'model' OR model_id IS NOT NULL)
);
-- Idempotent leaf writes: a retried job sees its record already exists (spec §6.3).
CREATE UNIQUE INDEX grading_records_leaf_uniq ON grading_records (run_id, answer_id, model_id)
    WHERE run_id IS NOT NULL;
CREATE INDEX grading_records_answer_idx ON grading_records (answer_id);

-- Per-leaf execution state: progress = GROUP BY state (restart-safe), and the
-- retry-failed operator action re-enqueues only failed items (D12).
CREATE TABLE grading_run_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES grading_runs (id) ON DELETE CASCADE,
    answer_id BIGINT NOT NULL REFERENCES answers (id) ON DELETE CASCADE,
    model_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'skipped')),
    attempts INT NOT NULL DEFAULT 0,
    error TEXT,
    record_id BIGINT REFERENCES grading_records (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, answer_id, model_id)
);
CREATE INDEX grading_run_items_run_state_idx ON grading_run_items (run_id, state);

-- The official grade is a guarded pointer (D6), now that records exist.
ALTER TABLE answers ADD CONSTRAINT answers_official_record_fk
    FOREIGN KEY (official_record_id) REFERENCES grading_records (id);

-- +goose Down
ALTER TABLE answers DROP CONSTRAINT answers_official_record_fk;
DROP TABLE grading_run_items;
DROP TABLE grading_records;
DROP TABLE grading_runs;
DROP TABLE grading_method_versions;
DROP TABLE grading_methods;
DROP TABLE prompt_template_versions;
