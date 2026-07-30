-- Per-student page (design doc 2026-07-28-student-page-design.md): the read-only,
-- staff-facing view of ONE student's history — exam cards with official totals, and
-- the story around them (publish state, provenance, regrades). Zero mutations.
--
-- PII (CLAUDE.md, D14): these queries deliberately never select
-- regrade_request_problems.complaint_text or regrade_requests.body/subject — student
-- content of the same class as transcriptions. Verdicts, statuses, ids, numbers and
-- timestamps only. Everything here is keyed by the INTERNAL students.id; the handler
-- resolves the school id once via GetStudentByExternalID.
--
-- EFFECTIVE-GRADE RESOLVER. The `overlay` LATERAL repeated below is the canonical
-- resolver already reproduced in ExportRows and AssessmentStudentTotals (review.sql),
-- PublishSnapshotInputs (publish.sql) and ContestedAnswerForSubItem (regrade.sql):
-- the latest ADOPTED regrade overlay wins over the round-0 official, live publish
-- chain first, then most-recently adjudicated (rounds design, 0028). Keep all of them
-- in sync — sqlc has no cross-query CTE/view reuse, so the duplication is the price.
--
-- Why effective and not the bare official pointer: this page is linked FROM the
-- Totals table and must show the same number, and the publish snapshots it diffs
-- against were THEMSELVES built from effective grades (PublishSnapshotInputs) — so
-- comparing a snapshot against a bare official would report a false "changed" for any
-- adoption that happened before the publish.

-- StudentAssessmentSummaries is the collapsed-card query: one row per assessment the
-- student has at least one answers row in (an assessment they never sat is not a
-- blank card — it is absent), newest first. Archived assessments are included on
-- purpose: history is history.
--
-- total is SQL NULL until something is effectively graded (D3 — never a fake 0);
-- `graded` counts answers with an effective record (a record with a NULL total, the
-- D12 refusal path, still counts as graded, matching AssessmentStudentTotals).
-- max is Σ max_points over EVERY problem of the assessment — the denominator the
-- student is measured against, independent of which answers exist.
-- published is "this student has an item in a live (non-superseded) batch" (D29) —
-- the same condition that decides whether the detail endpoint has a publish object.
-- name: StudentAssessmentSummaries :many
SELECT
    ass.id AS assessment_id,
    ass.name,
    ass.kind,
    ass.created_at,
    count(a.id) AS answers,
    count(eff.id) AS graded,
    sum(eff.total)::numeric AS total,
    coalesce((SELECT sum(p.max_points) FROM problems p WHERE p.assessment_id = ass.id), 0)::numeric AS max,
    EXISTS (
        SELECT 1
        FROM publish_items pi
        JOIN publish_batches pb ON pb.id = pi.batch_id
        WHERE pb.assessment_id = ass.id
          AND pb.superseded_at IS NULL
          AND pi.student_id = sqlc.arg(student_id)::bigint
    )::bool AS published
FROM answers a
JOIN assessments ass ON ass.id = a.assessment_id
LEFT JOIN LATERAL (
    SELECT rp.adopted_record_id AS record_id
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    LEFT JOIN publish_items pi2 ON pi2.id = rr.publish_item_id
    LEFT JOIN publish_batches pb2 ON pb2.id = pi2.batch_id
    WHERE rr.assessment_id = a.assessment_id
      AND rr.student_id = a.student_id
      AND rp.problem_id = a.problem_id
      AND rp.verdict = 'regraded'
      AND rp.adopted_record_id IS NOT NULL
    ORDER BY (pb2.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC
    LIMIT 1
) overlay ON true
LEFT JOIN grading_records eff ON eff.id = coalesce(overlay.record_id, a.official_record_id)
WHERE a.student_id = sqlc.arg(student_id)::bigint
GROUP BY ass.id
ORDER BY ass.created_at DESC, ass.id DESC;

-- StudentAssessmentProblems is the problem rows for every card the query above
-- returns, in one round trip. It walks PROBLEMS (not answers) so it covers every
-- problem of the assessment in number order: a problem with no answers row for this
-- student comes back with answer_id and score NULL, which the page renders as
-- "absent" and does not link. Restricted to the assessments the student actually has
-- answers in, matching StudentAssessmentSummaries' card set exactly.
-- name: StudentAssessmentProblems :many
SELECT
    p.assessment_id,
    p.number AS problem_number,
    p.title,
    p.max_points,
    a.id AS answer_id,
    eff.total AS score
FROM problems p
LEFT JOIN answers a ON a.problem_id = p.id AND a.student_id = sqlc.arg(student_id)::bigint
LEFT JOIN LATERAL (
    SELECT rp.adopted_record_id AS record_id
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    LEFT JOIN publish_items pi2 ON pi2.id = rr.publish_item_id
    LEFT JOIN publish_batches pb2 ON pb2.id = pi2.batch_id
    WHERE rr.assessment_id = p.assessment_id
      AND rr.student_id = sqlc.arg(student_id)::bigint
      AND rp.problem_id = p.id
      AND rp.verdict = 'regraded'
      AND rp.adopted_record_id IS NOT NULL
    ORDER BY (pb2.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC
    LIMIT 1
) overlay ON true
LEFT JOIN grading_records eff ON eff.id = coalesce(overlay.record_id, a.official_record_id)
WHERE p.assessment_id IN (
    SELECT a2.assessment_id FROM answers a2 WHERE a2.student_id = sqlc.arg(student_id)::bigint
)
ORDER BY p.assessment_id, p.number;

-- StudentAssessmentProblemDetail is the expanded card's provenance row per problem:
-- who graded it (human/model + model id), how confident, the answer's flags, and the
-- current effective score. source is coalesced to the empty string rather than left
-- NULL because sqlc types a NOT NULL column as non-nullable even on the nullable side
-- of a LEFT JOIN — the handler maps empty back to JSON null (the same workaround
-- ExportRows uses for official_source).
-- Provenance describes the record that produced the DISPLAYED score (the adopted
-- overlay when there is one): showing round 0's provenance beside a round-1 score
-- would be exactly the lie-by-omission the design doc calls out.
-- name: StudentAssessmentProblemDetail :many
SELECT
    p.number AS problem_number,
    a.id AS answer_id,
    coalesce(a.flags, '{}')::text [] AS flags,
    coalesce(eff.source, '')::text AS source,
    eff.model_id,
    eff.confidence,
    eff.total AS score
FROM problems p
LEFT JOIN answers a ON a.problem_id = p.id AND a.student_id = sqlc.arg(student_id)::bigint
LEFT JOIN LATERAL (
    SELECT rp.adopted_record_id AS record_id
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    LEFT JOIN publish_items pi2 ON pi2.id = rr.publish_item_id
    LEFT JOIN publish_batches pb2 ON pb2.id = pi2.batch_id
    WHERE rr.assessment_id = p.assessment_id
      AND rr.student_id = sqlc.arg(student_id)::bigint
      AND rp.problem_id = p.id
      AND rp.verdict = 'regraded'
      AND rp.adopted_record_id IS NOT NULL
    ORDER BY (pb2.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC
    LIMIT 1
) overlay ON true
LEFT JOIN grading_records eff ON eff.id = coalesce(overlay.record_id, a.official_record_id)
WHERE p.assessment_id = sqlc.arg(assessment_id)::bigint
ORDER BY p.number;

-- StudentLivePublishItem is the student's latest item in a LIVE (non-superseded)
-- batch for this assessment (D29) — the snapshot of what the student was actually
-- told, plus the delivery facts around it. Zero rows ⇒ the page shows no publish
-- section at all. Same selection rule as LiveBatchItemForStudent (publish.sql); this
-- one additionally carries the batch's created_at, which the card displays.
-- name: StudentLivePublishItem :one
SELECT
    pi.id,
    pi.snapshot,
    pi.recipient_email,
    pi.email_status,
    pi.sent_at,
    pb.created_at AS batch_created_at
FROM publish_items pi
JOIN publish_batches pb ON pb.id = pi.batch_id
WHERE pb.assessment_id = sqlc.arg(assessment_id)::bigint
  AND pb.superseded_at IS NULL
  AND pi.student_id = sqlc.arg(student_id)::bigint
ORDER BY pi.batch_id DESC
LIMIT 1;

-- StudentRegradeRequests lists this (student, assessment)'s regrade threads, newest
-- first. status is the request's OWN status column (migrations 0017/0025/0028:
-- received | under_review | resolved_upheld | resolved_regraded | rejected_*), passed
-- through verbatim — the page never invents a vocabulary the queue does not use.
-- No body/subject/resolution_note: student content stays in the database.
-- name: StudentRegradeRequests :many
SELECT rr.id, rr.received_at, rr.status
FROM regrade_requests rr
WHERE rr.assessment_id = sqlc.arg(assessment_id)::bigint
  AND rr.student_id = sqlc.arg(student_id)::bigint
ORDER BY rr.received_at DESC, rr.id DESC;

-- StudentRegradeProblems is the per-problem verdict line for the requests above.
-- verdict is NULL while a sub-item is still in progress (migration 0025) — the page
-- shows that as "pending", never as an outcome. complaint_text is NOT selected.
-- name: StudentRegradeProblems :many
SELECT rp.request_id, p.number AS problem_number, rp.verdict
FROM regrade_request_problems rp
JOIN problems p ON p.id = rp.problem_id
WHERE rp.request_id = ANY (sqlc.arg(request_ids)::bigint [])
ORDER BY rp.request_id, p.number;
