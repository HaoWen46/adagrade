-- +goose Up
-- Transcription cache for the LaTeX export (spec
-- 2026-07-25-latex-transcription-export-design.md §2). The export is explicitly
-- NOT part of grading, and must never re-spend on work already paid for, so a
-- transcription is content-addressed: the same masked pages under the same
-- model, prompt version, and decoding params are transcribed ONCE, ever.
--
-- Blocks (not LaTeX) are what gets cached. The emitter is a pure function of
-- these rows, so improving the .tex output re-exports the whole cohort for free.
--
-- prompt_version is part of the key on purpose: a prompt change alters what the
-- model was asked for, so reusing an older row under a newer contract would
-- silently mix two different products.
CREATE TABLE answer_transcriptions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    answer_id BIGINT NOT NULL REFERENCES answers (id) ON DELETE CASCADE,
    -- Content address: the exact masked page SHAs that were sent, in page order.
    -- A re-scan changes the SHAs and therefore needs a new transcription.
    image_shas TEXT [] NOT NULL,
    model_id TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    params_hash TEXT NOT NULL, -- temperature + any other decoding knobs
    blocks JSONB NOT NULL,     -- []transcribe.Block
    confidence TEXT NOT NULL CHECK (confidence IN ('high', 'medium', 'low', 'illegible')),
    -- B-C10 mask-quality signal: how much roster identity had to be scrubbed out
    -- of this transcription. Counts only, never the text.
    redaction_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
    input_tokens INT,
    output_tokens INT,
    cost_usd NUMERIC(10, 6),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (answer_id, image_shas, model_id, prompt_version, params_hash)
);

CREATE INDEX answer_transcriptions_answer_idx ON answer_transcriptions (answer_id);

-- +goose Down
DROP TABLE answer_transcriptions;
