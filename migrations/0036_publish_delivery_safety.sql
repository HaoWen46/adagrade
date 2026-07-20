-- +goose Up
-- A publish item's old pending/sent/failed lifecycle could not distinguish work
-- merely queued from work that had crossed the provider side-effect boundary. Add
-- a generation-bound delivery state machine so stale River jobs lose a CAS, and so
-- a crash after provider invocation is quarantined as uncertain instead of being
-- sent again automatically.
ALTER TABLE publish_items DROP CONSTRAINT publish_items_email_status_check;

ALTER TABLE publish_items
    ADD COLUMN email_generation INTEGER NOT NULL DEFAULT 1
        CHECK (email_generation > 0),
    ADD COLUMN delivery_key UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN delivery_job_id BIGINT,
    ADD COLUMN delivery_state_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- A legacy pending row may have had no job, a queued job, or a provider call whose
-- success was never recorded. There is no durable evidence that distinguishes the
-- three EXCEPT one case (audit A2-migration): a River email.send job for this
-- exact item that is still queued/runnable AND has never been attempted (attempt
-- = 0) proves the crash happened before River ever picked the job up — River will
-- simply run the original job after restart, so there is no duplicate-delivery
-- risk to acknowledge. Everything else — no job, an exhausted/terminal job, or a
-- job already attempted at least once (River sets attempt before invoking the
-- worker, so attempt > 0 means a provider call may already have happened) — fails
-- closed the same as before: require an operator to acknowledge duplicate-delivery
-- risk before creating a new generation.
--
-- river_job is River's own schema, created by River's migrator at startup AFTER
-- these goose migrations run (CLAUDE.md) — on a truly fresh database it does not
-- exist yet here, and a fresh database also cannot have any pre-existing
-- publish_items rows to match against. Guard with to_regclass so that ordering
-- doesn't turn a first-ever boot into a migration failure.
-- +goose StatementBegin
DO $$
BEGIN
    IF to_regclass('river_job') IS NOT NULL THEN
        UPDATE publish_items pi
        SET email_status = 'uncertain'
        WHERE pi.email_status = 'pending'
          AND NOT EXISTS (
              SELECT 1 FROM river_job rj
              WHERE rj.kind = 'email.send'
                AND rj.attempt = 0
                AND rj.state IN ('available', 'scheduled', 'running', 'retryable')
                AND rj.args @> jsonb_build_object('item_id', pi.id)
          );
    ELSE
        UPDATE publish_items SET email_status = 'uncertain' WHERE email_status = 'pending';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE publish_items ADD CONSTRAINT publish_items_email_status_check
    CHECK (email_status IN (
        'pending', 'claimed', 'sending', 'uncertain', 'sent', 'failed', 'skipped'
    ));

CREATE UNIQUE INDEX publish_items_delivery_key_idx ON publish_items (delivery_key);
CREATE UNIQUE INDEX publish_items_delivery_job_idx ON publish_items (delivery_job_id)
    WHERE delivery_job_id IS NOT NULL;
CREATE INDEX publish_items_delivery_state_idx
    ON publish_items (email_status, delivery_state_at);

-- +goose Down
DROP INDEX publish_items_delivery_state_idx;
DROP INDEX publish_items_delivery_job_idx;
DROP INDEX publish_items_delivery_key_idx;

ALTER TABLE publish_items DROP CONSTRAINT publish_items_email_status_check;

-- The pre-0036 schema has no safe representation of an in-flight or ambiguous
-- provider call. Preserve the rows but make them terminal instead of converting
-- them to pending and risking an automatic duplicate send after rollback.
UPDATE publish_items
SET email_status = 'failed',
    error = coalesce(error, 'delivery outcome requires manual review after migration rollback')
WHERE email_status IN ('claimed', 'sending', 'uncertain');

ALTER TABLE publish_items
    DROP COLUMN delivery_state_at,
    DROP COLUMN delivery_job_id,
    DROP COLUMN delivery_key,
    DROP COLUMN email_generation;

ALTER TABLE publish_items ADD CONSTRAINT publish_items_email_status_check
    CHECK (email_status IN ('pending', 'sent', 'failed', 'skipped'));
