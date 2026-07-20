-- Publish + outbound grade email (spec §2, §8 — Phase 6). Per-assessment,
-- snapshot-based, append-only, matching the grading-records philosophy.
--
--   publish_batches   one row per publish action (assessment, actor, note, timing).
--   publish_items     one row per included roster student: snapshot JSONB (per-problem
--                     totals, per-criterion scores + comments, assessment total),
--                     recipient email (roster email AT PUBLISH TIME — B-H10), regrade
--                     token, email send status.
--
-- Locking continues to use answers.published_at (added in 0003) — trigger-free, per
-- the design doc. This migration only adds the batch/item bookkeeping around it.

-- +goose Up
CREATE TABLE publish_batches (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    note TEXT NOT NULL DEFAULT '',
    resend_all BOOLEAN NOT NULL DEFAULT FALSE, -- D30: false = changed-only re-publish
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_at TIMESTAMPTZ -- D29 unpublish: stamped, batch never deleted
);
CREATE INDEX publish_batches_assessment_idx ON publish_batches (assessment_id, id DESC);

CREATE TABLE publish_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES publish_batches (id) ON DELETE CASCADE,
    student_id BIGINT NOT NULL REFERENCES students (id),
    snapshot JSONB NOT NULL, -- per-problem totals, per-criterion scores+comments, total (Q3 builds shape)
    recipient_email TEXT NOT NULL, -- roster email at publish time (B-H10)
    regrade_token TEXT NOT NULL DEFAULT '',
    email_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (email_status IN ('pending', 'sent', 'failed', 'skipped')),
    provider_message_id TEXT,
    sent_at TIMESTAMPTZ,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, student_id)
);
CREATE INDEX publish_items_batch_idx ON publish_items (batch_id);
CREATE INDEX publish_items_student_idx ON publish_items (student_id);
CREATE INDEX publish_items_status_idx ON publish_items (batch_id, email_status);

-- +goose Down
DROP TABLE publish_items;
DROP TABLE publish_batches;
