-- Publish batches/items (spec §2, §7 — Phase 6).

-- name: CreatePublishBatch :one
INSERT INTO publish_batches (assessment_id, note, resend_all, created_by, attachment, zip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: CreatePublishItem :one
INSERT INTO publish_items (batch_id, student_id, snapshot, recipient_email, regrade_token, email_status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: SetPublishedAtForAssessment :exec
UPDATE answers SET published_at = now(), updated_at = now()
WHERE assessment_id = $1 AND published_at IS NULL;

-- SupersedePublishBatch only succeeds when $1 is the LATEST non-superseded batch for
-- its own assessment (D29 unpublish escape hatch). Superseding an older batch would
-- otherwise clear published_at assessment-wide out from under a newer, still-live
-- batch. Zero rows back means either the batch is already superseded or a newer batch
-- exists for the same assessment -- store.SupersedePublishBatch turns that into
-- store.ErrNotLatestBatch.
-- name: SupersedePublishBatch :one
UPDATE publish_batches pb SET superseded_at = now()
WHERE pb.id = $1 AND pb.superseded_at IS NULL
  AND pb.id = (
      SELECT max(pb2.id) FROM publish_batches pb2
      WHERE pb2.assessment_id = (SELECT pb3.assessment_id FROM publish_batches pb3 WHERE pb3.id = $1)
        AND pb2.superseded_at IS NULL
  )
RETURNING *;

-- name: ClearPublishedAtForAssessment :exec
UPDATE answers SET published_at = NULL, updated_at = now()
WHERE assessment_id = $1;

-- name: GetPublishBatch :one
SELECT * FROM publish_batches WHERE id = $1;

-- GetPublishBatchForUpdate serializes unpublish/resend with the delivery worker's
-- transition across the external provider boundary.
-- name: GetPublishBatchForUpdate :one
SELECT * FROM publish_batches WHERE id = $1 FOR UPDATE;

-- name: ListPublishBatches :many
SELECT * FROM publish_batches WHERE assessment_id = $1 ORDER BY id DESC;

-- name: LatestNonSupersededBatch :one
SELECT * FROM publish_batches
WHERE assessment_id = $1 AND superseded_at IS NULL
ORDER BY id DESC LIMIT 1;

-- name: ListPublishItems :many
SELECT pi.*, st.student_id AS student_external_id, st.name AS student_name
FROM publish_items pi
JOIN students st ON st.id = pi.student_id
WHERE pi.batch_id = $1
ORDER BY st.student_id;

-- PublishBatchItemCounts is the per-batch email-status breakdown for the batches-
-- history summary (B5 audit finding): the batches-list endpoint previously carried
-- only the full items[] list, so a lecturer reading a collapsed summary row (e.g.
-- "ITEMS 14 / SENT 10 / FAILED 0 / UNCERTAIN 0") had no account of the other 4 —
-- all-no_submission or none-provider "skipped" items were only visible after
-- expanding the row, and the row as printed read as 4 lost emails. skipped is now a
-- first-class count alongside the rest.
-- name: PublishBatchItemCounts :one
SELECT
    count(*) AS items,
    count(*) FILTER (WHERE email_status = 'sent') AS sent,
    count(*) FILTER (WHERE email_status = 'failed') AS failed,
    count(*) FILTER (WHERE email_status = 'uncertain') AS uncertain,
    count(*) FILTER (WHERE email_status = 'skipped') AS skipped
FROM publish_items
WHERE batch_id = $1;

-- name: GetPublishItem :one
SELECT * FROM publish_items WHERE id = $1;

-- GetPublishItemForSend loads everything the email_send job needs in one row: the
-- item (snapshot, recipient, status), its batch's corrected/superseded flags, the
-- batch's attachment/zip settings (migration 0022, spec §3 -- the send job builds the
-- report PDF/ZIP attachment per these batch-level settings), and the assessment name
-- for the subject line. `corrected` is true when any earlier batch exists for the
-- assessment (re-publish ⇒ "corrected results", spec §2/§3); `batch_superseded` lets
-- the job skip an item whose batch was unpublished mid-flight.
-- name: GetPublishItemForSend :one
SELECT pi.id, pi.batch_id, pi.student_id, pi.snapshot, pi.recipient_email,
    pi.regrade_token, pi.email_status, pi.email_generation, pi.delivery_key,
    pi.delivery_job_id, pi.delivery_state_at,
    pb.assessment_id, pb.attachment, pb.zip,
    (pb.superseded_at IS NOT NULL)::bool AS batch_superseded,
    asr.name AS assessment_name,
    EXISTS (
        SELECT 1 FROM publish_batches older
        WHERE older.assessment_id = pb.assessment_id AND older.id < pb.id
    ) AS corrected,
    st.name AS student_name, st.student_id AS student_external_id
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
JOIN assessments asr ON asr.id = pb.assessment_id
JOIN students st ON st.id = pi.student_id
WHERE pi.id = $1;

-- GetPublishItemForResend loads one item plus its parent batch's attachment settings
-- (spec §4 D46 individual resend, §9/§10 attachment/zip fields added in migration
-- 0022) -- the single row the resend action needs to reconstruct the same attachment
-- behaviour as the original send without re-deriving it from scratch.
-- name: GetPublishItemForResend :one
SELECT pi.id, pi.batch_id, pi.student_id, pi.snapshot, pi.recipient_email,
    pi.regrade_token, pi.email_status, pi.email_generation, pi.delivery_key,
    pi.delivery_job_id, pi.delivery_state_at,
    pb.assessment_id, pb.attachment, pb.zip,
    (pb.superseded_at IS NOT NULL)::bool AS batch_superseded,
    asr.name AS assessment_name,
    st.name AS student_name, st.student_id AS student_external_id
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
JOIN assessments asr ON asr.id = pb.assessment_id
JOIN students st ON st.id = pi.student_id
WHERE pi.id = $1;

-- name: PublishItemsByStatus :many
SELECT pi.*, st.student_id AS student_external_id, st.name AS student_name
FROM publish_items pi
JOIN students st ON st.id = pi.student_id
WHERE pi.batch_id = $1 AND pi.email_status = $2
ORDER BY st.student_id;

-- name: UpdatePublishItemEmailStatus :exec
UPDATE publish_items
SET email_status = $2,
    provider_message_id = sqlc.narg(provider_message_id),
    error = sqlc.narg(error),
    sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE sent_at END,
    delivery_state_at = now()
WHERE id = $1;

-- ArmPublishItemResend starts a new delivery generation. A normal resend is
-- deliberately limited to terminal, known-outcome states. An uncertain attempt may
-- only be re-armed when the caller explicitly acknowledges duplicate-delivery risk.
-- Pending/claimed/sending never match, so a double click cannot replace an active
-- job. The generation CAS makes concurrent resend requests deterministic.
-- name: ArmPublishItemResend :one
UPDATE publish_items
SET email_status = 'pending',
    email_generation = email_generation + 1,
    delivery_key = gen_random_uuid(),
    delivery_job_id = NULL,
    delivery_state_at = now(),
    provider_message_id = NULL,
    sent_at = NULL,
    error = NULL
WHERE id = $1
  AND email_generation = $2
  AND (
      email_status IN ('sent', 'failed', 'skipped')
      OR (email_status = 'uncertain' AND sqlc.arg(allow_uncertain)::boolean)
  )
RETURNING *;

-- ClaimPublishItemDelivery lets exactly one River job own one generation. The job
-- id is persisted by the CAS itself, so duplicate jobs for the same generation lose
-- before any message is built or sent.
-- name: ClaimPublishItemDelivery :one
UPDATE publish_items
SET email_status = 'claimed',
    delivery_job_id = $3,
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND email_status = 'pending'
  AND delivery_job_id IS NULL
RETURNING *;

-- ClaimLegacyUncertainPublishItemDelivery rescues a migration-0036-backfilled row
-- (audit A2): 0036 added delivery_job_id and, in the same migration, blindly flipped
-- every still-pending row to 'uncertain' before any application code ever set that
-- column -- so email_status='uncertain' AND delivery_job_id IS NULL can only be that
-- exact legacy shape. Every post-0036 write path that marks a row uncertain
-- (MarkPublishItemDeliveryUncertain) requires delivery_job_id to already equal a
-- concrete job id, so it can never produce this shape. The caller further restricts
-- this to a job's first attempt (job.Attempt == 1): that job has never once invoked
-- the provider, and pre-0036 enqueue created exactly one job per item, so the send
-- provably never happened. Sets delivery_job_id/claims exactly as
-- ClaimPublishItemDelivery does for an ordinary pending row.
-- name: ClaimLegacyUncertainPublishItemDelivery :one
UPDATE publish_items
SET email_status = 'claimed',
    delivery_job_id = $3,
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND email_status = 'uncertain'
  AND delivery_job_id IS NULL
RETURNING *;

-- name: GetPublishBatchForItemForUpdate :one
SELECT pb.*
FROM publish_batches pb
JOIN publish_items pi ON pi.batch_id = pb.id
WHERE pi.id = $1
FOR UPDATE OF pb;

-- GetPublishItemStudentForUpdate defines the withdrawal/send race at the durable
-- provider boundary. BeginPublishItemSending locks the batch first, then this roster
-- row; whichever of withdrawal or this lock wins determines whether delivery may
-- proceed.
-- name: GetPublishItemStudentForUpdate :one
SELECT st.*
FROM students st
JOIN publish_items pi ON pi.student_id = st.id
WHERE pi.id = $1
FOR UPDATE OF st;

-- SkipWithdrawnPublishItemDelivery terminates the exact claimed generation while
-- the caller holds both its batch and student locks. No recipient or other PII is
-- recorded in the reason.
-- name: SkipWithdrawnPublishItemDelivery :one
UPDATE publish_items
SET email_status = 'skipped',
    delivery_job_id = NULL,
    error = 'student withdrawn before delivery',
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'claimed'
RETURNING *;

-- BeginPublishItemSendingCAS is called only after the store wrapper has locked and
-- verified both the parent batch and student row. Keeping all three operations in
-- one transaction serializes this side-effect boundary with unpublish/withdrawal.
-- name: BeginPublishItemSendingCAS :one
UPDATE publish_items
SET email_status = 'sending', delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'claimed'
RETURNING *;

-- name: MarkPublishItemDeliverySent :one
UPDATE publish_items
SET email_status = 'sent',
    provider_message_id = sqlc.narg(provider_message_id),
    sent_at = now(),
    error = sqlc.narg(error),
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'sending'
RETURNING id;

-- MarkPublishItemDeliveryFailed is valid on either side of the provider boundary,
-- but callers must name the state they observed. The explicit allow-list prevents
-- this generic expected-state parameter from overwriting a terminal/newer state.
-- name: MarkPublishItemDeliveryFailed :one
UPDATE publish_items
SET email_status = 'failed',
    provider_message_id = NULL,
    error = $5,
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = $4
  AND $4::text IN ('claimed', 'sending')
RETURNING id;

-- MarkPublishItemDeliveryUncertain quarantines a generation after the provider
-- boundary when the caller cannot prove whether the external side effect happened.
-- It is never auto-retried; ArmPublishItemResend requires explicit acknowledgement.
-- name: MarkPublishItemDeliveryUncertain :one
UPDATE publish_items
SET email_status = 'uncertain',
    provider_message_id = sqlc.narg(provider_message_id),
    error = sqlc.arg(error_text),
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'sending'
RETURNING id;

-- ReleasePublishItemDeliveryClaim is safe only before the provider boundary. It
-- clears ownership so the River retry of this generation can claim afresh.
-- name: ReleasePublishItemDeliveryClaim :one
UPDATE publish_items
SET email_status = 'pending',
    delivery_job_id = NULL,
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'claimed'
RETURNING id;

-- ReleasePublishItemSending is ONLY for a typed provider result that proves the
-- request was not accepted. Untyped errors/timeouts after the provider boundary are
-- ambiguous and must transition to uncertain instead.
-- name: ReleasePublishItemSending :one
UPDATE publish_items
SET email_status = 'pending',
    delivery_job_id = NULL,
    delivery_state_at = now()
WHERE id = $1
  AND email_generation = $2
  AND delivery_job_id = $3
  AND email_status = 'sending'
RETURNING id;

-- name: HasSendingPublishItems :one
SELECT EXISTS (
    SELECT 1 FROM publish_items
    WHERE batch_id = $1 AND email_status = 'sending'
)::boolean;

-- SkipUndeliveredPublishItemsForBatch terminates queued/claimed generations when
-- their batch is unpublished before they cross the provider boundary. The caller
-- holds the parent batch lock and has already proved that no item is sending.
-- name: SkipUndeliveredPublishItemsForBatch :execrows
UPDATE publish_items
SET email_status = 'skipped',
    delivery_job_id = NULL,
    error = 'batch unpublished before delivery',
    delivery_state_at = now()
WHERE batch_id = $1 AND email_status IN ('pending', 'claimed');

-- SetPublishItemRegradeToken persists the signed token minted from the item id at
-- send time (spec §4: stored on the item for display/debug; verification is by
-- recomputation, not lookup, so this row is not on the security path).
-- name: SetPublishItemRegradeToken :exec
UPDATE publish_items SET regrade_token = $2 WHERE id = $1;

-- ResetPublishItemToPending flips a failed item back to pending and clears its error
-- so the resend-failed action (spec §7) can re-enqueue it. Guarded to email_status =
-- 'failed' so a concurrent send that already moved the item to 'sent' is not clobbered
-- back to pending. Returns the affected id (0 rows ⇒ it was no longer failed).
-- name: ResetPublishItemToPending :one
UPDATE publish_items
SET email_status = 'pending', error = NULL
WHERE id = $1 AND email_status = 'failed'
RETURNING id;

-- name: LatestBatchItemsByStudent :many
-- The most recent non-superseded batch's items, keyed by student — used for
-- changed-only re-publish diffing (D30).
SELECT pi.*
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
WHERE pb.assessment_id = $1 AND pb.superseded_at IS NULL
  AND pb.id = (
      SELECT id FROM publish_batches
      WHERE assessment_id = $1 AND superseded_at IS NULL
      ORDER BY id DESC LIMIT 1
  );

-- LatestBatchItemsByStudentAny is the changed-only diff BASELINE (D30): each
-- student's MOST RECENT item across ALL of the assessment's batches (superseded or
-- not), keyed by student.
--
-- It must be per-student-across-batches, not "the newest batch's items", because a
-- changed-only re-publish writes a THIN batch containing only the students who
-- changed. If the baseline were just the newest batch, the next cycle's diff would
-- see every student absent from that thin batch as `!ok → changed` and re-email the
-- whole cohort (C1). DISTINCT ON keeps only the highest batch_id row per student, so
-- a student who last appeared three batches ago still contributes their real last
-- published snapshot as the baseline. Re-publish always follows an unpublish (the
-- lock forces it), so superseded batches are deliberately included.
-- name: LatestBatchItemsByStudentAny :many
SELECT DISTINCT ON (pi.student_id) pi.*
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
WHERE pb.assessment_id = $1
ORDER BY pi.student_id, pi.batch_id DESC;

-- LatestBatchAny returns the most recent batch (superseded or not) — the presence of
-- any prior batch means a subsequent publish is a re-publish (changed-only default).
-- name: LatestBatchAny :one
SELECT * FROM publish_batches WHERE assessment_id = $1 ORDER BY id DESC LIMIT 1;

-- LiveBatchItemForStudent returns one student's item in the assessment's CURRENT
-- non-superseded (live) batch, if any — the freshest published snapshot for that
-- student. Used by the regrade resolution email (C2): a "regraded" outcome's "New
-- total" should reflect the LIVE published figure (a changed-only re-publish picks the
-- student up after a regrade), not the superseded snapshot the token was minted
-- against. Zero rows ⇒ no live batch, or the student wasn't in it (a thin changed-only
-- batch), and the caller falls back to computing from current official grades.
-- name: LiveBatchItemForStudent :one
SELECT pi.*
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
WHERE pb.assessment_id = $1 AND pb.superseded_at IS NULL AND pi.student_id = $2
ORDER BY pi.batch_id DESC
LIMIT 1;

-- StudentOfficialTotalForAssessment computes a student's current official total (Σ of
-- their official records' totals) and max (Σ of every problem's max_points for the
-- assessment), straight from live grading state. The regrade resolution email (C2)
-- uses this as the second-choice source for a "regraded" New total when there is no
-- live publish item for the student. graded counts how many of the student's answers
-- have an official record — zero means nothing is official yet, so the caller omits
-- the total line rather than announcing "0/max".
-- name: StudentOfficialTotalForAssessment :one
SELECT
    coalesce(sum(gr.total), 0)::numeric AS total,
    coalesce((SELECT sum(p.max_points) FROM problems p WHERE p.assessment_id = $1), 0)::numeric AS max,
    count(a.official_record_id) AS graded
FROM answers a
LEFT JOIN grading_records gr ON gr.id = a.official_record_id
WHERE a.assessment_id = $1 AND a.student_id = $2;

-- Coverage gate (spec §2): every (roster student x problem) answer for the
-- assessment must have an official grade or be effectively no_submission (no pages
-- uploaded). Rows here are the BLOCKERS -- answers with no official grade and at
-- least one page (i.e. not simply un-submitted), UNIONed with a second, distinct
-- blocker kind: active roster students who have ZERO answers rows for this assessment
-- at all (roster-add after ingest, or an ingest that silently never materialized their
-- answers -- see PublishSnapshotInputs/PublishCoverageCounts, which both start FROM
-- answers and so are blind to a student with no answers row; this query and
-- PublishCoverageCounts fail closed instead by also walking students LEFT JOIN
-- answers). kind='ungraded' rows carry an answer_id/problem; kind='not_ingested' rows
-- carry only the student (answer_id 0, problem_number 0, problem_title '') so the
-- operator can tell "grade this answer" apart from "re-ingest or fix the roster for
-- this student". Both arms exclude withdrawn students (roster-lifecycle plan
-- 2026-07-10: a 停修 student's ungraded pages must not block everyone else's
-- results; their already-published history is untouched elsewhere).
-- name: PublishBlockers :many
SELECT a.id AS answer_id, st.student_id AS student_external_id, st.name AS student_name,
    p.number AS problem_number, p.title AS problem_title, 'ungraded'::text AS kind
FROM answers a
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
WHERE a.assessment_id = $1
  AND st.withdrawn_at IS NULL
  AND a.official_record_id IS NULL
  AND EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)
UNION ALL
SELECT 0 AS answer_id, st.student_id AS student_external_id, st.name AS student_name,
    0::int AS problem_number, '' AS problem_title, 'not_ingested'::text AS kind
FROM students st
WHERE st.withdrawn_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM answers a WHERE a.assessment_id = $1 AND a.student_id = st.id)
ORDER BY student_external_id, problem_number;

-- PublishCoverageCounts counts ACTIVE students' answers only (all arms join out
-- withdrawn students, roster-lifecycle plan 2026-07-10): a withdrawn student's
-- answers neither block (their ungraded pages are not `blocked`) nor count toward
-- graded/no_submission/total — mirroring PublishBlockers and PublishSnapshotInputs
-- so preview counts, blocker rows, and snapshot contents always describe the same
-- population.
-- name: PublishCoverageCounts :one
SELECT
    count(*) AS total_answers,
    count(*) FILTER (WHERE a.official_record_id IS NOT NULL) AS graded,
    count(*) FILTER (WHERE a.official_record_id IS NULL
        AND NOT EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)) AS no_submission,
    count(*) FILTER (WHERE a.official_record_id IS NULL
        AND EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)) AS blocked,
    -- Roster-add-after-ingest gap (fail closed, see PublishBlockers above): active
    -- roster students with zero answers rows for this assessment. Counted separately
    -- from `blocked` (which is answer-scoped) but folded into total blocking by the
    -- caller (store.PublishPreviewRow.Publishable).
    (SELECT count(*) FROM students st2
     WHERE st2.withdrawn_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM answers a2 WHERE a2.assessment_id = $1 AND a2.student_id = st2.id)
    ) AS not_ingested,
    -- B3: problem_count lets the caller size not_ingested's "missing cells" (one
    -- not-ingested student is missing answers for EVERY problem, not just one) when
    -- building a coverage-percentage denominator — see store.PublishPreview.
    (SELECT count(*) FROM problems p WHERE p.assessment_id = $1) AS problem_count
FROM answers a
JOIN students st ON st.id = a.student_id
WHERE a.assessment_id = $1
  AND st.withdrawn_at IS NULL;

-- Per-criterion metadata (name, max points, position) for every rubric criterion
-- referenced by an official record of the assessment. The snapshot JSONB stores
-- per-criterion score+comment lines (spec §2), but grading_records.criterion_scores
-- only carries criterion_id → score; Q3 joins these rows in Go to attach the
-- human-readable description and max. DISTINCT because two answers can pin the same
-- rubric version.
-- name: PublishCriteria :many
-- Covers both the round-0 official records' rubric versions AND any regrade-overlay
-- adopted records' rubric versions (regrade-round correctness fix): the snapshot now
-- renders per-criterion lines from the EFFECTIVE (possibly adopted) record, so its
-- criterion metadata must be resolvable too. A round AI record pins the contested
-- official's rubric so the two usually coincide, but a manual-fallback adopted record
-- may pin a different version — union both so no criterion name goes blank.
SELECT DISTINCT rc.id AS criterion_id, rc.rubric_version_id, rc.position,
    rc.description, rc.points
FROM rubric_criteria rc
WHERE rc.rubric_version_id IN (
    SELECT gr.rubric_version_id
    FROM answers a
    JOIN grading_records gr ON gr.id = a.official_record_id
    WHERE a.assessment_id = $1
    UNION
    SELECT agr.rubric_version_id
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    JOIN grading_records agr ON agr.id = rp.adopted_record_id
    WHERE rr.assessment_id = $1
      AND rp.verdict = 'regraded'
      AND rp.adopted_record_id IS NOT NULL
)
ORDER BY rc.rubric_version_id, rc.position;

-- AnswerIDForStudentProblem resolves one (assessment, student, problem number) to its
-- current answer_id, LIVE — the send job's attachment build looks up each snapshot
-- problem's answer this way instead of trusting a persisted answer_id (finding 1: a
-- persisted answer_id broke the changed-only republish diff for pre-existing
-- snapshots that predate the field, so it was removed from the stored JSONB; the
-- (assessment_id, student_id, problem_id) unique index on answers, joined through
-- problems.number, makes this resolution both stable and deterministic across
-- send/resend — answers are pre-materialized and never re-keyed for a given
-- (student, problem)). No rows means the answer was never materialized (e.g. a
-- no_submission problem some ingest paths never create a row for) — the caller
-- treats that the same as an empty attachment section for that problem.
-- name: AnswerIDForStudentProblem :one
SELECT a.id FROM answers a
JOIN problems p ON p.id = a.problem_id
WHERE a.assessment_id = $1 AND a.student_id = $2 AND p.number = $3;

-- Per-student snapshot inputs: one row per (student, problem) with the EFFECTIVE
-- record's criterion scores/comment/total, plus roster email. Go composes these rows
-- into the per-student snapshot JSONB (Q3 owns the shape). Withdrawn students are
-- excluded (roster-lifecycle plan 2026-07-10): NEW batches carry no item and send no
-- email for them; items in already-created batches are untouched.
--
-- EFFECTIVE, not round-0 official (regrade-round correctness fix): the snapshot that
-- backs the grade email, report PDF, and changed-only re-publish diff must show the
-- same figure ExportRows/AssessmentStudentTotals do — the latest adopted regrade
-- overlay over the round-0 official — else a republish/resend re-emails the pre-regrade
-- grade and an adoption never flags the student as changed. The overlay LATERAL is
-- byte-identical to ExportRows' (see that query's note); the effective record is then
-- joined once on COALESCE(overlay.record_id, a.official_record_id).
-- name: PublishSnapshotInputs :many
SELECT st.id AS student_id, st.student_id AS student_external_id, st.name AS student_name,
    st.email AS student_email,
    p.id AS problem_id, p.number AS problem_number, p.title AS problem_title, p.max_points,
    a.id AS answer_id,
    (NOT EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id))::bool AS no_submission,
    egr.id AS record_id, egr.criterion_scores, egr.total, egr.comment
FROM answers a
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
LEFT JOIN LATERAL (
    SELECT rp.adopted_record_id AS record_id
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    LEFT JOIN publish_items pi ON pi.id = rr.publish_item_id
    LEFT JOIN publish_batches pb ON pb.id = pi.batch_id
    WHERE rr.assessment_id = a.assessment_id
      AND rr.student_id = a.student_id
      AND rp.problem_id = a.problem_id
      AND rp.verdict = 'regraded'
      AND rp.adopted_record_id IS NOT NULL
    ORDER BY (pb.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC
    LIMIT 1
) overlay ON true
LEFT JOIN grading_records egr ON egr.id = COALESCE(overlay.record_id, a.official_record_id)
WHERE a.assessment_id = $1
  AND st.withdrawn_at IS NULL
ORDER BY st.student_id, p.number;
