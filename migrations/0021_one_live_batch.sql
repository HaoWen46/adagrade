-- Single-live-batch invariant at the DB level (M2). Publish already refuses a second
-- publish while a non-superseded batch exists (ErrAlreadyPublished), but that guard is a
-- check-then-act: two concurrent publish calls can both pass the hasLiveBatch read and
-- both create a live batch, leaving LatestNonSupersededBatch (and thus Unpublish)
-- ambiguous. A partial unique index makes the DB the source of truth — at most one
-- non-superseded batch per assessment at any instant. store.CreatePublishBatch maps the
-- resulting unique-violation (23505) to ErrAlreadyPublished so the race loses cleanly
-- with the same 409 the pre-check produces.
--
-- Superseded batches are excluded from the index (WHERE superseded_at IS NULL), so the
-- append-only history of past batches is unaffected — only the live one is constrained.

-- +goose Up
CREATE UNIQUE INDEX publish_batches_one_live_per_assessment
    ON publish_batches (assessment_id) WHERE superseded_at IS NULL;

-- +goose Down
DROP INDEX publish_batches_one_live_per_assessment;
