-- Regrade v2 schema (design doc 2026-07-03-regrade-v2-design.md §9, task W1): one
-- request row per inbound email (kind filed|addendum|unparsed|handed_off), per-problem
-- sub-items table, per-problem TA assignment, and the partial unique index that kills
-- the concurrent-double-reply race structurally (D57) instead of via a count-based
-- rate cap.
--
-- This is a REPLACE, not an extension: v1's request-level problem_id/ai_record_id/
-- escalated columns are dropped. Pre-production (no live tokens per the design doc
-- preamble), so no shim/back-compat path is kept. In particular, whatever value v1's
-- escalated column held (including escalated=true rows) is discarded by design going
-- forward -- the escalation band it tracked is retired in v2 (spec §5/§6 replace it
-- with per-problem TA assignment), and there is no live data to preserve.
--
--   regrade_request_problems  One row per contested problem within a request (spec
--                             §5 D59): complaint text, its own AI record/error, its
--                             own verdict. UNIQUE(request_id, problem_id) -- a TA's
--                             escape-hatch add/correct (§5) must not create a second
--                             sub-item for the same problem on the same request.
--   problem_ta_assignments    At most one TA per problem (spec §6 D60); one TA may
--                             own many problems, hence UNIQUE on problem_id alone,
--                             not a composite.
--   regrade_requests.kind     filed (>=1 valid block, token consumed) | addendum
--                             (reply to an already-consumed token, no processing) |
--                             unparsed (0 valid blocks, token NOT consumed) |
--                             handed_off (consumed the final-turn token -- see design
--                             doc §6). Backfill: every existing row -> 'filed' by
--                             default (v1 had no addendum/unparsed/handed_off concept;
--                             whatever exists pre-production came from the v1 flow's
--                             "verified" path), EXCEPT rows with no publish_item_id
--                             and/or no turn (token-never-parsed or
--                             rejected-before-a-turn-was-assigned rows, both legal in
--                             v1 -- 0017, 0023) -- those get corrected to 'unparsed'
--                             since 'filed' would violate the new
--                             regrade_requests_filed_needs_slot CHECK below.
--   partial unique index      (publish_item_id, turn) WHERE kind = 'filed' -- the
--                             race-killer (D57): a token is consumed exactly once by
--                             the request that manages to INSERT ... kind='filed'
--                             first; two racing filers on the same token collide on
--                             this index, and the loser's insert is caught by the
--                             caller and re-recorded as an addendum. addendum/unparsed/
--                             handed_off rows are unaffected (index only applies to
--                             kind='filed'), so a token CAN accumulate many addendum/
--                             unparsed rows -- only one filed row per (item, turn).
--
-- HARD LESSON from migration 0024 (broken twice in review, see
-- regrade_ai_source_migration_test.go): Down must unlink FK references and delete
-- dependent rows BEFORE dropping/re-narrowing anything else. Down here: drop the new
-- tables first (regrade_request_problems, problem_ta_assignments -- both pure
-- additions, no data to preserve going backwards), THEN drop the partial index and
-- kind column, THEN re-add problem_id/ai_record_id/escalated with NULL/FALSE
-- defaults (v1 shape had no backfill requirement going down -- the columns simply
-- come back empty, which is correct: v2 rows never populated them).

-- +goose Up

-- Per-problem sub-items (spec §5 D59). request_id has no ON DELETE CASCADE:
-- regrade_requests rows are never deleted (append-only audit history), so the
-- question doesn't arise, but we're explicit rather than silent about it by using the
-- same plain REFERENCES style as regrade_requests' own FKs (0017).
CREATE TABLE regrade_request_problems (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id BIGINT NOT NULL REFERENCES regrade_requests (id),
    problem_id BIGINT NOT NULL REFERENCES problems (id),
    complaint_text TEXT NOT NULL DEFAULT '', -- student content: same PII class as regrade_requests.body, never logged
    ai_record_id BIGINT REFERENCES grading_records (id), -- per-sub-item AI re-grade result (spec §5: re-scoped, one job per sub-item)
    ai_error TEXT, -- terminal AI failure reason, short constant string only (CLAUDE.md PII rule) -- mirrors v1's request-level ai_error (0024)
    verdict TEXT CHECK (verdict IN ('upheld', 'regraded')), -- NULL until a TA adjudicates this problem
    verdict_note TEXT NOT NULL DEFAULT '',
    verdict_by BIGINT REFERENCES users (id),
    verdict_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, problem_id)
);
CREATE INDEX regrade_request_problems_request_idx ON regrade_request_problems (request_id);
-- AllProblemsVerdicted / send-result gate (spec §5): "does this request have any
-- sub-item still unverdicted" is the hot query behind the 409 gate.
CREATE INDEX regrade_request_problems_unverdicted_idx ON regrade_request_problems (request_id) WHERE verdict IS NULL;

-- Per-problem TA assignment (spec §6 D60): at most one TA per problem (UNIQUE on
-- problem_id alone -- not composite), one TA may own many problems.
CREATE TABLE problem_ta_assignments (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    problem_id BIGINT NOT NULL REFERENCES problems (id),
    user_id BIGINT NOT NULL REFERENCES users (id),
    assigned_by BIGINT REFERENCES users (id),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (problem_id)
);
-- ListTAAssignments(assessmentID) joins problems -> problem_ta_assignments; problems
-- is already indexed on assessment_id via its own PK/FK shape (0002), so no extra
-- index needed here beyond the UNIQUE(problem_id) above, which also serves lookups by
-- problem_id (GetProblemTA).
CREATE INDEX problem_ta_assignments_user_idx ON problem_ta_assignments (user_id);

-- regrade_requests: add kind, drop the v1 single-problem/single-AI-record/escalated
-- columns (turn stays -- spec §9). kind starts NOT NULL DEFAULT 'filed' so the
-- backfill below is really just documentation of intent; every pre-existing row
-- becomes 'filed' (v1 had no addendum/unparsed/handed_off concept -- see header).
ALTER TABLE regrade_requests ADD COLUMN kind TEXT NOT NULL DEFAULT 'filed'
    CHECK (kind IN ('filed', 'addendum', 'unparsed', 'handed_off'));
ALTER TABLE regrade_requests ALTER COLUMN kind DROP DEFAULT; -- default was only a backfill convenience; new inserts must say what they are

-- Correct the blanket 'filed' backfill for rows that never had a real (item, turn)
-- slot -- v1 allowed publish_item_id NULL ("token didn't even parse", 0017) and turn
-- NULL (rejected-before-consuming-a-turn rows never got a turn number, 0023's
-- backfill). Those rows have no v2 kind that fits perfectly either, but 'filed' is
-- actively wrong for them (and, as of the CHECK immediately below, no longer even
-- insertable) -- 'unparsed' is the closest honest fit: no slot was ever consumed.
UPDATE regrade_requests SET kind = 'unparsed'
    WHERE kind = 'filed' AND (publish_item_id IS NULL OR turn IS NULL);

-- The race-killer (D57): only one 'filed' row may exist per (publish_item_id, turn).
-- Partial so addendum/unparsed/handed_off rows on the same token never collide with
-- each other or with the filed row.
CREATE UNIQUE INDEX regrade_requests_filed_item_turn_uniq
    ON regrade_requests (publish_item_id, turn) WHERE kind = 'filed';

-- Self-enforcing companion to the partial index above (review finding): a partial
-- unique index silently disengages for NULL columns -- Postgres treats NULLs as
-- distinct, so two 'filed' rows both carrying NULL publish_item_id/turn would both
-- satisfy the index and both succeed, defeating D57's race-killer for a caller bug
-- that inserts kind='filed' without a real slot. The intended flow always has
-- turn>=1 and publish_item_id set for a filed row, so make that invariant a hard
-- database constraint rather than trusting every caller to populate it correctly.
ALTER TABLE regrade_requests ADD CONSTRAINT regrade_requests_filed_needs_slot
    CHECK (kind <> 'filed' OR (publish_item_id IS NOT NULL AND turn IS NOT NULL));

ALTER TABLE regrade_requests DROP COLUMN problem_id;
ALTER TABLE regrade_requests DROP COLUMN ai_record_id;
ALTER TABLE regrade_requests DROP COLUMN escalated;

-- +goose Down

-- New tables first -- pure additions in Up, nothing else references them, so no
-- unlinking needed before dropping them (unlike 0024's regrade_ai FK trap).
DROP TABLE problem_ta_assignments;
DROP TABLE regrade_request_problems;

DROP INDEX regrade_requests_filed_item_turn_uniq;
-- Explicit DROP CONSTRAINT rather than relying on DROP COLUMN kind's implicit cascade:
-- regrade_requests persists across this Down (only new columns/tables are removed),
-- so unlike the two new tables above, this constraint's removal isn't a side effect of
-- dropping the table it lives on -- it must be torn down by name before/alongside the
-- column it references.
ALTER TABLE regrade_requests DROP CONSTRAINT regrade_requests_filed_needs_slot;
ALTER TABLE regrade_requests DROP COLUMN kind;

-- Re-add the v1 columns v2 replaced. Going down, v2 rows never populated these, so
-- they simply come back empty (NULL/FALSE) -- no data to preserve, no FK-unlink dance
-- required here (unlike 0024, where the down direction had to delete rows that WOULD
-- violate a re-narrowed CHECK; here the re-added columns start with no data at all).
ALTER TABLE regrade_requests ADD COLUMN escalated BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE regrade_requests ADD COLUMN ai_record_id BIGINT REFERENCES grading_records (id);
ALTER TABLE regrade_requests ADD COLUMN problem_id BIGINT REFERENCES problems (id);
