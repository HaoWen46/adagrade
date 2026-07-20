-- +goose Up
-- A method final source is an immutable grading RUN, not the logical method.
-- A method may gain versions and later partial runs; following the method made
-- officials silently mix records from different executions while publish's
-- spot-check gate inspected only the newest completed run.

ALTER TABLE assessments
    DROP CONSTRAINT assessments_final_source_check,
    ADD COLUMN final_run_id BIGINT;

-- Preserve the old UI/server choice deterministically by pinning the exact run
-- the pre-0035 publish gate regarded as authoritative: the latest completed run
-- for the assessment and selected method.
WITH picked AS (
    SELECT DISTINCT ON (a.id)
        a.id AS assessment_id,
        r.id AS run_id
    FROM assessments a
    JOIN grading_runs r ON r.assessment_id = a.id
    JOIN grading_method_versions mv ON mv.id = r.method_version_id
    WHERE a.final_source_kind = 'method'
      AND mv.method_id = a.final_method_id
      AND r.status = 'completed'
    ORDER BY a.id, r.id DESC
)
UPDATE assessments a
SET final_run_id = picked.run_id
FROM picked
WHERE picked.assessment_id = a.id;

-- Second rung (audit A6): rung 1 above only recovers the standard case — the
-- LATEST completed run for the assessment's DECLARED method. A legacy
-- assessment may have since moved to a different method, or its "latest"
-- completed run for that method may not be the one that actually produced
-- the officials still sitting on its answers (e.g. a later run was launched
-- but never selected). Recover the run that DOES back the assessment's
-- current officials before giving up: the run most represented among
-- official_record_id rows with source='model', i.e. the run a human (or the
-- pre-0035 code path) actually treated as authoritative. Trust that
-- evidence over the declared method, updating final_method_id to the
-- recovered run's real method — final_method_id is otherwise meaningless
-- without a matching final_run_id.
--
-- Require the winning candidate to independently pass every guard
-- SetAssessmentFinalSource enforces at runtime (run belongs to this
-- assessment — ErrFinalRunInvalid; audit A3/A4-minimal: completed status,
-- assessment scope, at least one succeeded leaf), so this backfill can
-- never pin a run the live code path would itself reject.
-- Anything that fails those checks (or has no official model records to
-- vote at all) falls through to the NULL fail-closed branch below.
WITH votes AS (
    SELECT a.id AS assessment_id, gr.run_id, count(*) AS n
    FROM assessments a
    JOIN answers ans ON ans.assessment_id = a.id
    JOIN grading_records gr
        ON gr.id = ans.official_record_id AND gr.source = 'model'
    WHERE a.final_source_kind = 'method'
      AND a.final_run_id IS NULL
    GROUP BY a.id, gr.run_id
),
best AS (
    SELECT DISTINCT ON (assessment_id) assessment_id, run_id
    FROM votes
    ORDER BY assessment_id, n DESC, run_id DESC
),
safe AS (
    SELECT best.assessment_id, best.run_id, mv.method_id
    FROM best
    -- Ownership: grading_records.run_id and .answer_id are independent FKs
    -- with no cross-check, so a legacy/corrupted official can vote for a
    -- run belonging to a DIFFERENT assessment. The live path rejects that
    -- as ErrFinalRunInvalid; this backfill must too, or it would silently
    -- pin a foreign run (and its foreign method) as the final source.
    JOIN grading_runs r
        ON r.id = best.run_id AND r.assessment_id = best.assessment_id
    JOIN grading_method_versions mv ON mv.id = r.method_version_id
    WHERE r.status = 'completed'
      AND r.scope_kind = 'assessment'
      AND EXISTS (
          SELECT 1 FROM grading_run_items gri
          WHERE gri.run_id = r.id AND gri.state = 'succeeded'
      )
)
UPDATE assessments a
SET final_run_id = safe.run_id,
    final_method_id = safe.method_id,
    updated_at = now()
FROM safe
WHERE safe.assessment_id = a.id;

-- A legacy method choice with no completed run — and no recoverable vote from
-- rung 2 above — cannot identify an exact source. Fail closed and make the
-- operator select a completed run explicitly again.
UPDATE assessments
SET final_source_kind = NULL,
    final_method_id = NULL,
    updated_at = now()
WHERE final_source_kind = 'method' AND final_run_id IS NULL;

-- Officials for newly-unset sources are no longer official. Published answers
-- remain immutable: their batch snapshot/history must not be rewritten by a
-- migration.
UPDATE answers a
SET official_record_id = NULL,
    official_set_by = NULL,
    official_set_at = NULL,
    updated_at = now()
FROM assessments ass
WHERE ass.id = a.assessment_id
  AND ass.final_source_kind IS NULL
  AND a.published_at IS NULL
  AND a.official_record_id IS NOT NULL;

-- Re-derive every unpublished method-sourced answer from the ONE pinned run.
-- Human current-rubric records remain the fallback when that run is silent or
-- the answer is flagged, preserving the round-based grading contract.
WITH picks AS (
    SELECT ans.id AS answer_id,
        COALESCE(CASE WHEN cardinality(ans.flags) = 0 THEN src.id END, hum.id) AS rec_id
    FROM answers ans
    JOIN assessments ass ON ass.id = ans.assessment_id
    LEFT JOIN LATERAL (
        SELECT gr.id
        FROM grading_records gr
        WHERE gr.answer_id = ans.id
          AND gr.run_id = ass.final_run_id
          AND gr.source = 'model'
          AND gr.total IS NOT NULL
          AND gr.rubric_version_id = (
              SELECT rv.id FROM rubric_versions rv
              WHERE rv.problem_id = ans.problem_id
              ORDER BY rv.version DESC
              LIMIT 1)
        ORDER BY gr.id DESC
        LIMIT 1
    ) src ON true
    LEFT JOIN LATERAL (
        SELECT gr.id
        FROM grading_records gr
        WHERE gr.answer_id = ans.id
          AND gr.source = 'human'
          AND gr.total IS NOT NULL
          AND gr.rubric_version_id = (
              SELECT rv.id FROM rubric_versions rv
              WHERE rv.problem_id = ans.problem_id
              ORDER BY rv.version DESC
              LIMIT 1)
        ORDER BY gr.id DESC
        LIMIT 1
    ) hum ON true
    WHERE ass.final_source_kind = 'method'
      AND ans.published_at IS NULL
)
UPDATE answers a
SET official_record_id = picks.rec_id,
    official_set_by = NULL,
    official_set_at = CASE WHEN picks.rec_id IS NULL THEN NULL ELSE now() END,
    updated_at = now()
FROM picks
WHERE a.id = picks.answer_id
  AND a.official_record_id IS DISTINCT FROM picks.rec_id;

ALTER TABLE assessments
    ADD CONSTRAINT assessments_final_source_check CHECK (
        (final_source_kind IS NULL AND final_method_id IS NULL AND final_run_id IS NULL)
        OR (final_source_kind = 'consensus' AND final_method_id IS NULL AND final_run_id IS NULL)
        OR (final_source_kind = 'method' AND final_method_id IS NOT NULL AND final_run_id IS NOT NULL)
    );

-- The selected run belongs to the assessment that owns it, creating an
-- intentional assessment -> run -> assessment cycle. Deferring the FK lets an
-- assessment deletion cascade through its runs before the reference is checked.
-- Add it after the data backfill so no deferred trigger events are pending when
-- the strict source CHECK above is installed.
ALTER TABLE assessments
    ADD CONSTRAINT assessments_final_run_fk
        FOREIGN KEY (final_run_id) REFERENCES grading_runs (id)
        DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX assessments_final_run_idx
    ON assessments (final_run_id)
    WHERE final_run_id IS NOT NULL;

-- +goose Down
DROP INDEX assessments_final_run_idx;

ALTER TABLE assessments
    DROP CONSTRAINT assessments_final_source_check,
    DROP COLUMN final_run_id,
    ADD CONSTRAINT assessments_final_source_check
        CHECK ((final_source_kind = 'method') = (final_method_id IS NOT NULL));
