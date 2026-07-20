-- Regrade turn/escalation ladder + problem-match + AI-assist linkage (spec §6-8 —
-- morning round D48/D49/D50/D52).
--
--   turn          1 + the count of EARLIER verified (non-rejected) requests for the
--                 same (student, assessment), ordered by id (spec §6). Nullable: rows
--                 whose token never parsed (ladder rung 1 failure) have no known
--                 student/assessment and so no meaningful turn.
--   escalated     true for verified requests beyond ADAMARKER_REGRADE_MAX (turn >
--                 MAX) but at/under ADAMARKER_REGRADE_HARD_MAX -- visible in the
--                 queue, AI-assist disabled, "manual review required" badge (D48).
--   problem_id    best-effort reply-parse guess (spec §7, D49) -- nullable, TA-editable.
--   ai_record_id  links to the grading_records row produced by the AI re-grade job
--                 (spec §8, D50/D52) -- nullable until (if ever) run; never auto-official.
--
-- Backfill (this migration, not the application): turn is computed for every EXISTING
-- row that has a non-NULL student_id/assessment_id (i.e. the token parsed), using the
-- same "verified = NOT IN rejected_*" rule CountVerifiedRegrades already uses (spec
-- §6/§5 rung 5) and the same per-(student,assessment) partition, ordered by id --
-- matching the app's turn = 1 + prior verified count going forward. Rejected rows
-- (rejected_bad_token/superseded/sender_mismatch/rate_limited) never consumed a turn
-- and keep turn NULL. escalated is derived from the backfilled turn against the
-- CURRENT ADAMARKER_REGRADE_MAX default (3) -- see note below on why this is a
-- reasonable one-time default rather than an env-driven backfill.

-- +goose Up
ALTER TABLE regrade_requests ADD COLUMN turn INT;
ALTER TABLE regrade_requests ADD COLUMN escalated BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE regrade_requests ADD COLUMN problem_id BIGINT REFERENCES problems (id);
ALTER TABLE regrade_requests ADD COLUMN ai_record_id BIGINT REFERENCES grading_records (id);

-- Backfill turn: verified requests only (spec §5 rung 5's definition, same predicate
-- CountVerifiedRegrades uses), numbered 1..N within each (student_id, assessment_id)
-- partition in id order (chronological, since id is an identity column).
WITH verified AS (
    SELECT id,
        row_number() OVER (
            PARTITION BY student_id, assessment_id
            ORDER BY id
        ) AS turn
    FROM regrade_requests
    WHERE student_id IS NOT NULL
      AND assessment_id IS NOT NULL
      AND status NOT IN (
          'rejected_bad_token', 'rejected_superseded',
          'rejected_sender_mismatch', 'rejected_rate_limited'
      )
)
UPDATE regrade_requests rr
SET turn = verified.turn
FROM verified
WHERE rr.id = verified.id;

-- Backfill escalated using the ladder's current default MAX=3 (ADAMARKER_REGRADE_MAX):
-- a one-time snapshot, not env-driven, because a migration cannot read process env at
-- deploy time and re-deriving it on every future config change would be wrong anyway
-- (escalation is decided at ingest time, not retroactively recomputed when the operator
-- tunes the cap later). Rows already resolved/rejected keep their historical outcome
-- regardless of this flag -- escalated only gates the *AI-assist button* going forward
-- (spec §8 D52 eligibility), so a stale flag on a long-closed row is inert.
UPDATE regrade_requests SET escalated = TRUE WHERE turn > 3;

-- +goose Down
ALTER TABLE regrade_requests DROP COLUMN ai_record_id;
ALTER TABLE regrade_requests DROP COLUMN problem_id;
ALTER TABLE regrade_requests DROP COLUMN escalated;
ALTER TABLE regrade_requests DROP COLUMN turn;
