-- Drill-down read models. Status is DERIVED from facts at query time (D2) — no
-- status enum exists anywhere.

-- name: ProblemSummaries :many
SELECT p.id, p.number, p.title, p.max_points, p.position,
    count(a.id) AS answer_count,
    count(*) FILTER (WHERE EXISTS (SELECT 1 FROM answer_pages ap WHERE ap.answer_id = a.id)) AS with_pages,
    count(*) FILTER (WHERE a.official_record_id IS NOT NULL) AS official_set,
    count(*) FILTER (WHERE EXISTS (SELECT 1 FROM grading_records gr WHERE gr.answer_id = a.id AND gr.source = 'model')) AS ai_graded,
    count(*) FILTER (WHERE EXISTS (SELECT 1 FROM grading_records gr WHERE gr.answer_id = a.id AND gr.source = 'human')) AS human_graded,
    count(*) FILTER (WHERE cardinality(a.flags) > 0) AS flagged,
    count(*) FILTER (WHERE a.published_at IS NOT NULL) AS published
FROM problems p
LEFT JOIN answers a ON a.problem_id = p.id
WHERE p.assessment_id = $1
GROUP BY p.id
ORDER BY p.position, p.number;

-- name: ProblemStudentRows :many
SELECT a.id AS answer_id, st.student_id, st.name, st.email, a.flags, a.published_at,
    a.official_record_id,
    (SELECT count(*) FROM answer_pages ap WHERE ap.answer_id = a.id) AS page_count,
    (SELECT count(*) FROM grading_records gr WHERE gr.answer_id = a.id) AS record_count,
    ogr.total AS official_total,
    ogr.source AS official_source
FROM answers a
JOIN students st ON st.id = a.student_id
LEFT JOIN grading_records ogr ON ogr.id = a.official_record_id
WHERE a.problem_id = $1
ORDER BY st.student_id;

-- name: GetAnswerContext :one
SELECT a.*,
    st.student_id AS student_external_id, st.name AS student_name, st.email AS student_email,
    p.number AS problem_number, p.title AS problem_title, p.max_points, p.statement,
    ass.name AS assessment_name
FROM answers a
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
JOIN assessments ass ON ass.id = a.assessment_id
WHERE a.id = $1;

-- name: ListRecordsForAnswer :many
SELECT * FROM grading_records WHERE answer_id = $1 ORDER BY id DESC;

-- name: GetRecord :one
SELECT * FROM grading_records WHERE id = $1;

-- name: StudentSubmissionRows :many
SELECT a.id AS answer_id, p.number AS problem_number, p.title AS problem_title,
    (SELECT count(*) FROM grading_records gr WHERE gr.answer_id = a.id) AS record_count,
    a.official_record_id,
    ap.id AS page_id, ap.page_index,
    (ap.masked_image_ref IS NOT NULL)::bool AS masked
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN students st ON st.id = a.student_id
LEFT JOIN answer_pages ap ON ap.answer_id = a.id
WHERE a.assessment_id = $1 AND st.student_id = $2
ORDER BY p.position, p.number, ap.page_index;

-- ExportRows exports EFFECTIVE grades (rounds design, 0028): the latest
-- adopted regrade overlay wins over the round-0 official. Withdrawn students
-- are NOT filtered — exports must never silently drop a student
-- (roster-lifecycle plan 2026-07-10) — they are flagged via `withdrawn`
-- instead, which the CSV emits as a final status column.
--
-- The overlay LATERAL below is the ONE canonical effective-grade resolver,
-- reproduced byte-for-byte in AssessmentStudentTotals and
-- PublishSnapshotInputs (and, with an extra exclude clause, in
-- ContestedAnswerForSubItem) — keep the four in sync. Ordering (regrade-round
-- correctness fix): turn numbers RESTART per publish chain, so a superseded
-- chain's high-turn adoption must not outrank the live chain's newer,
-- lower-turn one. So resolve by (1) live chain first — the request's publish
-- item's batch is NOT superseded — then (2) most-recently adjudicated
-- (verdict_at), then turn/id as stable tiebreaks. `rp.verdict = 'regraded'`
-- guards against a stale adopted_record_id left behind by a later flip to
-- 'upheld' (only a regraded verdict's adoption is an effective grade).
-- name: ExportRows :many
SELECT st.student_id, st.name, p.number AS problem_number,
    COALESCE(ogr.total, gr.total)::numeric AS total,
    COALESCE(ogr.source, gr.source, '')::text AS official_source,
    (st.withdrawn_at IS NOT NULL)::bool AS withdrawn
FROM answers a
JOIN students st ON st.id = a.student_id
JOIN problems p ON p.id = a.problem_id
LEFT JOIN grading_records gr ON gr.id = a.official_record_id
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
LEFT JOIN grading_records ogr ON ogr.id = overlay.record_id
WHERE a.assessment_id = $1
ORDER BY st.student_id, p.number;

-- AssessmentStudentTotals sums each student's EFFECTIVE grades (rounds design,
-- 0028): the latest adopted regrade-overlay record per answer wins over the
-- round-0 official; the official alone otherwise. NULL total until anything
-- effective exists (D3). Withdrawn students stay visible with `withdrawn`
-- true (never silently dropped, roster-lifecycle plan 2026-07-10) — the
-- totals card badges them instead. The overlay LATERAL is byte-identical to
-- ExportRows' (see that query's note on the cross-chain ordering fix).
-- name: AssessmentStudentTotals :many
SELECT st.student_id, st.name, st.email,
    (st.withdrawn_at IS NOT NULL)::bool AS withdrawn,
    count(a.id) AS answers,
    count(COALESCE(overlay.record_id, a.official_record_id)) AS graded,
    sum(COALESCE(ogr.total, gr.total))::numeric AS total
FROM answers a
JOIN students st ON st.id = a.student_id
LEFT JOIN grading_records gr ON gr.id = a.official_record_id
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
LEFT JOIN grading_records ogr ON ogr.id = overlay.record_id
WHERE a.assessment_id = $1
GROUP BY st.id
ORDER BY st.student_id;
