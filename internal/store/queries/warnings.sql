-- Workflow-warning counts (hazard audit 2026-07-10): cheap, assessment-scoped
-- COUNT queries backing GET /api/assessments/{id}/workflow-warnings and the
-- launch/publish preflight warnings. Counts only — no PII columns ever leave
-- these queries.

-- ScanPageStateCounts buckets an assessment's scan pages by the SAME derived-
-- state precedence as ScanBatchPageProgress (scan.sql — D2: status is derived,
-- never stored), collapsed to the buckets the warnings still read directly:
-- assigned-but-unpromoted (Finalize not clicked) and still-processing. The
-- stranded kinds (errored/parked/orphaned) moved to StrandedScanPageStats,
-- which also classifies them by cell coverage.
-- name: ScanPageStateCounts :one
SELECT
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NOT NULL
    ) AS assigned_unpromoted,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NULL
          AND identified_at IS NULL
    ) AS processing
FROM scan_pages
WHERE assessment_id = $1;

-- StrandedScanPageStats classifies an assessment's stranded scan pages —
-- errored, parked, orphaned, same derived-state precedence as
-- ScanBatchPageProgress (D2) — by whether the (student, problem) cell they
-- claim is already covered by a live submission (false-alarm fix 2026-07-11):
-- a failed batch superseded by a successful re-upload leaves dead pages behind,
-- and those must not warn "answers grade incomplete" when every cell is covered.
--
--   * uncovered_*: pages with a known cell (the assigned pair when assigned,
--     else the OCR-proposed pair when both halves exist) that NO live submission
--     covers — the real hazard: those answers grade incomplete or not at all.
--     uncovered_cells is the distinct (student, problem) count behind them.
--   * unidentified: stranded pages with no assigned or proposed identity — they
--     cannot be checked against coverage, so they stay worth flagging, but with
--     wording that never claims incompleteness.
--   * covered_pages: pages whose cell IS covered by a live submission — dead
--     leftovers (e.g. a superseded batch), informational only.
--
-- Coverage mirrors CountMissingCells (scan.sql): a live (non-superseded,
-- non-retracted) submission for the student, whole-assessment or per-problem.
-- Counts only — no PII columns ever leave this query.
-- name: StrandedScanPageStats :one
WITH stranded AS (
    SELECT
        CASE
            WHEN sp.error IS NOT NULL AND sp.error <> '' THEN 'errored'
            WHEN sp.parked_reason IS NOT NULL THEN 'parked'
            ELSE 'orphaned'
        END AS kind,
        CASE
            WHEN sp.assigned_student_id IS NOT NULL THEN sp.assigned_student_id
            WHEN sp.proposed_student_id IS NOT NULL AND sp.proposed_problem_id IS NOT NULL
                THEN sp.proposed_student_id
        END AS cell_student_id,
        CASE
            WHEN sp.assigned_student_id IS NOT NULL THEN sp.assigned_problem_id
            WHEN sp.proposed_student_id IS NOT NULL AND sp.proposed_problem_id IS NOT NULL
                THEN sp.proposed_problem_id
        END AS cell_problem_id
    FROM scan_pages sp
    WHERE sp.assessment_id = sqlc.arg(assessment_id)
      AND (
          (sp.error IS NOT NULL AND sp.error <> '')
          OR (
              sp.discarded_at IS NULL AND sp.submission_id IS NULL
              AND (
                  sp.parked_reason IS NOT NULL
                  OR (sp.assigned_student_id IS NULL AND sp.identified_at IS NOT NULL)
              )
          )
      )
),

classified AS (
    SELECT
        st.kind,
        st.cell_student_id,
        st.cell_problem_id,
        (st.cell_student_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM submissions s
            WHERE s.assessment_id = sqlc.arg(assessment_id)
              AND s.student_id = st.cell_student_id
              AND s.superseded_by IS NULL AND s.retracted_at IS NULL
              AND (s.problem_id IS NULL OR s.problem_id = st.cell_problem_id)
        )) AS covered
    FROM stranded st
)

SELECT
    count(*) FILTER (WHERE c.cell_student_id IS NOT NULL AND NOT c.covered) AS uncovered,
    count(*) FILTER (WHERE c.cell_student_id IS NOT NULL AND NOT c.covered AND c.kind = 'orphaned') AS uncovered_orphaned,
    count(*) FILTER (WHERE c.cell_student_id IS NOT NULL AND NOT c.covered AND c.kind = 'parked') AS uncovered_parked,
    count(*) FILTER (WHERE c.cell_student_id IS NOT NULL AND NOT c.covered AND c.kind = 'errored') AS uncovered_errored,
    (
        SELECT count(DISTINCT (c2.cell_student_id, c2.cell_problem_id))
        FROM classified c2
        WHERE c2.cell_student_id IS NOT NULL AND NOT c2.covered
    )::bigint AS uncovered_cells,
    count(*) FILTER (WHERE c.cell_student_id IS NULL) AS unidentified,
    count(*) FILTER (WHERE c.covered) AS covered_pages
FROM classified c;

-- name: CountOpenQuarantine :one
SELECT count(*) FROM upload_quarantine
WHERE assessment_id = $1 AND resolved_at IS NULL;

-- CountMaskErrorPages: pages whose MaskPage job failed terminally (mask_error,
-- migration 0015) — they may still carry an OLD accepted masked image, so a run
-- would send a stale/unmasked view to the provider.
-- name: CountMaskErrorPages :one
SELECT count(*)
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
WHERE a.assessment_id = $1 AND ap.mask_error IS NOT NULL;

-- CountSupersededGradedAnswers: answers whose images were force-replaced after
-- grading (ingest stamps the image_superseded flag, ingest.go) and that have at
-- least one grading record — those grades scored the OLD images.
-- name: CountSupersededGradedAnswers :one
SELECT count(*)
FROM answers a
WHERE a.assessment_id = $1
  AND 'image_superseded' = ANY (a.flags)
  AND EXISTS (SELECT 1 FROM grading_records gr WHERE gr.answer_id = a.id);

-- CountActiveRunsForAssessment: pending/running grading runs — backs both the
-- run_in_progress standing warning and the launch dialog's active_run_overlap.
-- name: CountActiveRunsForAssessment :one
SELECT count(*) FROM grading_runs
WHERE assessment_id = $1 AND status IN ('pending', 'running');

-- name: CountProblemsWithoutRubric :one
SELECT count(*)
FROM problems p
WHERE p.assessment_id = $1
  AND NOT EXISTS (SELECT 1 FROM rubric_versions rv WHERE rv.problem_id = p.id);

-- CountProblemsWithoutRubricByIDs is the launch-preflight variant scoped to the
-- run's problems (Task B2 resolves the scope to problem ids in Go).
-- name: CountProblemsWithoutRubricByIDs :one
SELECT count(*)
FROM problems p
WHERE p.id = ANY (sqlc.arg(problem_ids)::bigint [])
  AND NOT EXISTS (SELECT 1 FROM rubric_versions rv WHERE rv.problem_id = p.id);

-- MixedMethodVersionStats is now a corruption/legacy defense: a pinned run owns
-- exactly one method version, so healthy final officials can never span versions.
-- name: MixedMethodVersionStats :one
WITH officials AS (
    SELECT gr.method_version_id, gmv.version
    FROM answers a
    JOIN assessments ass ON ass.id = a.assessment_id
    JOIN grading_records gr ON gr.id = a.official_record_id
    JOIN grading_method_versions gmv ON gmv.id = gr.method_version_id
    WHERE a.assessment_id = $1
      AND ass.final_source_kind = 'method'
      AND gr.run_id = ass.final_run_id
)

SELECT
    (SELECT count(DISTINCT o.method_version_id) FROM officials o)::bigint AS distinct_versions,
    (
        SELECT count(*) FROM officials o
        WHERE o.version < (SELECT max(o2.version) FROM officials o2)
    )::bigint AS stale_answers;

-- FinalSourceModelRecordStats: backs the final_source_no_records danger code
-- (analysis redesign plan, Task B1). final_is_method says whether the exam's
-- chosen final source is a method at all (FALSE for consensus or undecided);
-- model_records counts the pinned run's source='model' records on this
-- assessment. Zero with final_is_method means deriving officials from
-- the source yields nothing, so publish would send holes.
-- name: FinalSourceModelRecordStats :one
SELECT
    COALESCE(ass.final_source_kind = 'method', FALSE)::bool AS final_is_method,
    (
        SELECT count(*)
        FROM grading_records gr
        JOIN answers a ON a.id = gr.answer_id
        WHERE a.assessment_id = ass.id
          AND gr.source = 'model'
          AND gr.run_id = ass.final_run_id
    )::bigint AS model_records
FROM assessments ass
WHERE ass.id = $1;

-- CountDuplicateActiveEmails: distinct emails shared by more than one ACTIVE
-- student (case-insensitive — email routing is), backing the duplicate_emails
-- danger warning (roster-lifecycle plan 2026-07-10): two students sharing an
-- address means grade emails land in the same mailbox. Blank emails are not
-- duplicates of each other. Count of EMAILS, not students, per the code's
-- vocabulary; no email values ever leave the query.
-- name: CountDuplicateActiveEmails :one
SELECT count(*) FROM (
    SELECT lower(email)
    FROM students
    WHERE withdrawn_at IS NULL AND email <> ''
    GROUP BY lower(email)
    HAVING count(*) > 1
) dup;

-- CountStudentsWithDuplicateNames: ACTIVE students sharing an exact name with
-- another active student — the duplicate_student_names info warning (their
-- pages can never auto-assign by name, see scan matchOCRName, so Identify
-- always needs manual confirmation for them). Count of STUDENTS (both/all
-- sharers), matching the code's "count = students sharing a name" contract.
-- name: CountStudentsWithDuplicateNames :one
SELECT count(*)
FROM students st
WHERE st.withdrawn_at IS NULL
  AND EXISTS (
      SELECT 1 FROM students st2
      WHERE st2.withdrawn_at IS NULL AND st2.id <> st.id AND st2.name = st.name
  );

-- CountUnmaterializedStudents: ACTIVE students with zero answers rows for this
-- assessment while the assessment already has at least one answers row (the
-- late-add dead end, roster-lifecycle plan 2026-07-10 — fix is the materialize
-- action). The ≥1-answers guard keeps the warning silent on a brand-new
-- assessment where NOBODY has been ingested yet.
-- name: CountUnmaterializedStudents :one
SELECT count(*)
FROM students st
WHERE st.withdrawn_at IS NULL
  AND EXISTS (SELECT 1 FROM answers any_a WHERE any_a.assessment_id = $1)
  AND NOT EXISTS (SELECT 1 FROM answers a WHERE a.assessment_id = $1 AND a.student_id = st.id);

-- CountAdjustedSpotChecksStillOfficial: spot-check verdicts recorded as
-- "adjusted" whose checked record is STILL the answer's official record — the
-- checker disagreed with the grade but the promised correction never landed
-- (runs.go handleSetSpotCheckVerdict records the verdict only; the grade edit
-- happens elsewhere, or not at all).
-- name: CountAdjustedSpotChecksStillOfficial :one
SELECT count(*)
FROM spot_checks sc
JOIN grading_records gr ON gr.id = sc.grading_record_id
JOIN answers a ON a.id = gr.answer_id
WHERE a.assessment_id = $1
  AND sc.verdict = 'adjusted'
  AND a.official_record_id = sc.grading_record_id;

-- CountTextRenderLossPages: pages whose PDF text layer contained runs that
-- rendered as nothing (render.ProbeTextLoss; pdfium drops non-embedded
-- CID/CJK glyphs silently). Counts live answer pages plus non-discarded scan
-- pages — a flagged scan page keeps warning even after promotion (the
-- promoted answer page is re-rasterized from the stored JPEG, which has no
-- text layer to probe).
-- name: CountTextRenderLossPages :one
SELECT
    (SELECT count(*)
     FROM answer_pages ap
     JOIN answers a ON a.id = ap.answer_id
     WHERE a.assessment_id = $1 AND ap.text_loss_runs > 0)
  + (SELECT count(*)
     FROM scan_pages sp
     WHERE sp.assessment_id = $1 AND sp.discarded_at IS NULL AND sp.text_loss_runs > 0)
  AS pages;
