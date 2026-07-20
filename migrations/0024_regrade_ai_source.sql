-- AI re-grade assist record source + policy (spec §8, D50/D51/D53).
--
-- The stricter AI re-grade (task B2) appends an ordinary grading_records row for the
-- contested answer, but it is neither a run leaf ('model') nor a TA edit ('human') —
-- it is a machine-authored, on-demand second opinion linked from a regrade_request
-- (regrade_requests.ai_record_id, migration 0023). It NEVER becomes official on its
-- own: the TA compares old vs new and walks the normal unpublish→official→re-publish
-- path. So it needs its own auditable source value 'regrade_ai', and its own pinned
-- policy value 'regrade_strict' (a single baked-in stance, not one of the three
-- curated D25 grading policies).
--
-- Three CHECK constraints touch these columns and all three must admit the new values:
--   grading_records_source_check (0009) — the source enum;
--   grading_records_check        (0009) — "source='model' OR created_by IS NOT NULL",
--                                          which a machine regrade_ai record (no
--                                          created_by) would otherwise violate;
--   the policy CHECK              (0012) — the nullable policy enum.
-- Mirrors how 0009 added 'aggregate': DROP + re-ADD each named constraint. Goose down
-- reverses, deleting any regrade_ai rows first so the narrower CHECK can re-apply.
--
-- Also adds regrade_requests.ai_error (spec §8): the visible terminal-failure surface
-- promised by the spec (e.g. "AI unavailable — provider removed") — 0023 added
-- ai_record_id for the success path but nothing carried the failure reason. This
-- migration is still unreleased, so the column is folded into it rather than a new
-- migration. Short constant strings only (never student/request text, CLAUDE.md PII
-- rule) — see internal/grading/regrade_assist.go.

-- +goose Up
ALTER TABLE grading_records DROP CONSTRAINT grading_records_source_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_source_check
    CHECK (source IN ('model', 'human', 'aggregate', 'regrade_ai'));

-- A regrade_ai record is machine-authored (no human created_by), like 'model' and
-- 'aggregate' — widen the created_by guard so it is not required for it.
ALTER TABLE grading_records DROP CONSTRAINT grading_records_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_check
    CHECK (source IN ('model', 'regrade_ai') OR created_by IS NOT NULL);

-- The policy enum (0012) gains the AI re-grade's baked-in stance. NULL is still legal
-- (human/aggregate records carry no policy); the three curated grading stances remain.
ALTER TABLE grading_records DROP CONSTRAINT grading_records_policy_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_policy_check
    CHECK (policy IS NULL OR policy IN ('lenient', 'standard', 'strict', 'regrade_strict'));

ALTER TABLE regrade_requests ADD COLUMN ai_error TEXT;

-- +goose Down
-- Unlink any regrade_requests that reference the regrade_ai records we're about to
-- delete. The FK constraint on regrade_requests.ai_record_id blocks the delete if any
-- record is still linked, so NULL the references first.
UPDATE regrade_requests SET ai_record_id = NULL WHERE ai_record_id IN (SELECT id FROM grading_records WHERE source='regrade_ai');

-- The delete MUST come after unlinking: regrade_ai rows carry policy='regrade_strict', which
-- violates the narrower policy CHECK re-added below. Re-narrowing before deleting
-- fails with a check-constraint violation whenever a regrade_ai row exists.
DELETE FROM grading_records WHERE source = 'regrade_ai';

ALTER TABLE regrade_requests DROP COLUMN ai_error;

ALTER TABLE grading_records DROP CONSTRAINT grading_records_policy_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_policy_check
    CHECK (policy IS NULL OR policy IN ('lenient', 'standard', 'strict'));

ALTER TABLE grading_records DROP CONSTRAINT grading_records_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_check
    CHECK (source = 'model' OR created_by IS NOT NULL);
ALTER TABLE grading_records DROP CONSTRAINT grading_records_source_check;
ALTER TABLE grading_records ADD CONSTRAINT grading_records_source_check
    CHECK (source IN ('model', 'human', 'aggregate'));
