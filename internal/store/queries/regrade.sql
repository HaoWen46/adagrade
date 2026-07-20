-- Regrade v2 (design doc 2026-07-03-regrade-v2-design.md §9, task W1): one request row
-- per inbound email (kind filed|addendum|unparsed|handed_off), per-problem sub-items in
-- regrade_request_problems, per-problem TA assignment in problem_ta_assignments. The v1
-- single problem_id/ai_record_id/escalated request-level columns are gone (migration
-- 0025) -- REPLACES v1's per-email-single-problem model, not additive.

-- name: InsertRegradeRequestV2 :one
-- message_id is the provider's per-delivery id (F1 idempotency key, unchanged from v1,
-- migration 0020): a partial unique index rejects a second insert carrying the same
-- non-empty message_id. kind/turn are set by the caller (the parser/webhook path):
-- turn NULL for rows whose token never parsed or that carry no turn concept
-- (kind='addendum' rows still carry the turn of the token they replied to). The
-- partial unique index on (publish_item_id, turn) WHERE kind='filed' (migration 0025,
-- D57) is the structural race-killer: a second concurrent INSERT ... kind='filed' for
-- the same (item, turn) fails as a unique violation, which the caller (IsUniqueViolation)
-- maps to "re-record this reply as an addendum instead", not a hard error.
INSERT INTO regrade_requests (
    publish_item_id, student_id, assessment_id, from_email,
    spf_verdict, dkim_verdict, subject, body, status, message_id,
    kind, turn
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetRegradeRequest :one
SELECT * FROM regrade_requests WHERE id = $1;

-- LockRegradeRequest serializes side effects that must happen at most once per
-- request but cannot be performed inside SQL (currently the reminder email send).
-- The caller must use this through transaction-bound Queries.
-- name: LockRegradeRequest :one
SELECT status, kind FROM regrade_requests WHERE id = $1 FOR UPDATE;

-- name: ConsumedTokenExists :one
-- true iff this (publish_item_id, turn) token has already been CONSUMED -- i.e. a row
-- exists that filed against it (kind='filed') OR that already fired the final-turn
-- handoff (kind='handed_off'). This is the consumed-token pre-check the webhook uses
-- (spec §4 D57): once a handoff row exists, MarkRequestHandedOff has flipped it OUT of
-- the partial unique index's WHERE kind='filed', freeing the raw index slot -- so a
-- FiledRequestExists-only check would wrongly let a SECOND reply to the MAX+1 token file
-- again. This query closes that gap by treating both consuming kinds as consumed. The
-- partial unique index still structurally guarantees at most one 'filed' row per slot
-- for the racing case; this read complements it for the handoff-already-happened case.
SELECT EXISTS (
    SELECT 1 FROM regrade_requests
    WHERE publish_item_id = $1 AND turn = $2 AND kind IN ('filed', 'handed_off')
) AS exists;

-- name: NextOpenTurn :one
-- The live chain's next open turn slot for a publish item: max consumed turn + 1 over
-- the rows that consume slots (kind IN ('filed','handed_off') — the same kinds
-- ConsumedTokenExists treats as consumed), COALESCEd to 1 for a chain with no filings
-- yet. Used by the webhook's rung-2 re-bind (wave-5 verifier finding): a superseded
-- chain's token must be REMAPPED to this slot rather than carrying its stale turn onto
-- the live chain — a preserved stale turn 2 would consume (live item, 2) before the
-- live chain ever sent result #1, stranding the student's later legitimate turn-2 reply
-- as a silent addendum. Addendum/unparsed rows never consume a slot, so they don't count.
SELECT (COALESCE(MAX(turn), 0) + 1)::int AS next_turn
FROM regrade_requests
WHERE publish_item_id = $1 AND kind IN ('filed', 'handed_off');

-- ListRegradeRequests / CountRegradeRequests share one WHERE: the queue page and
-- its pager total must always agree. student_prefix is a pre-escaped ILIKE
-- pattern (the store wrapper escapes \ % _ and appends %) matching the external
-- student ID; rows with no linked student (NULL FK) never match a student search.
-- student_withdrawn (roster-lifecycle plan 2026-07-10): withdrawn students keep
-- their regrade channel (停修 rights preserved), but the queue flags them so a
-- TA sees the status while adjudicating. False for rows with no linked student.
--
-- Status-GROUP filters (HCI audit, regrades-list correctness): the UI's default
-- "Actionable" queue is kind=filed AND an open status — a compound the single-value
-- status param can't express, which used to force fetch-unfiltered-narrow-client-side
-- (empty pages + lying totals once >1 page existed). Both nargs are true-or-NULL:
--   only_open: status IN (received, under_review). Combine with kind as needed.
--   only_undelivered_result: the resolved-but-never-delivered recovery set
--     (migration 0026) — kind='filed' is INSIDE the predicate because a reminded
--     unparsed row is also resolved_upheld with result_sent_at NULL, yet no result
--     email was ever owed there.
-- name: ListRegradeRequests :many
SELECT rr.*, st.student_id AS student_external_id, st.name AS student_name,
    (st.withdrawn_at IS NOT NULL)::bool AS student_withdrawn,
    a.name AS assessment_name
FROM regrade_requests rr
LEFT JOIN students st ON st.id = rr.student_id
LEFT JOIN assessments a ON a.id = rr.assessment_id
WHERE (sqlc.narg(status)::text IS NULL OR rr.status = sqlc.narg(status))
  AND (sqlc.narg(assessment_id)::bigint IS NULL OR rr.assessment_id = sqlc.narg(assessment_id))
  AND (sqlc.narg(kind)::text IS NULL OR rr.kind = sqlc.narg(kind))
  AND (sqlc.narg(student_prefix)::text IS NULL OR st.student_id ILIKE sqlc.narg(student_prefix))
  AND (sqlc.narg(only_open)::bool IS NULL
       OR (rr.status IN ('received', 'under_review')) = sqlc.narg(only_open))
  AND (sqlc.narg(only_undelivered_result)::bool IS NULL
       OR (rr.kind = 'filed'
           AND rr.status IN ('resolved_upheld', 'resolved_regraded')
           AND rr.result_sent_at IS NULL) = sqlc.narg(only_undelivered_result))
ORDER BY rr.id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountRegradeRequests :one
SELECT count(*)
FROM regrade_requests rr
LEFT JOIN students st ON st.id = rr.student_id
WHERE (sqlc.narg(status)::text IS NULL OR rr.status = sqlc.narg(status))
  AND (sqlc.narg(assessment_id)::bigint IS NULL OR rr.assessment_id = sqlc.narg(assessment_id))
  AND (sqlc.narg(kind)::text IS NULL OR rr.kind = sqlc.narg(kind))
  AND (sqlc.narg(student_prefix)::text IS NULL OR st.student_id ILIKE sqlc.narg(student_prefix))
  AND (sqlc.narg(only_open)::bool IS NULL
       OR (rr.status IN ('received', 'under_review')) = sqlc.narg(only_open))
  AND (sqlc.narg(only_undelivered_result)::bool IS NULL
       OR (rr.kind = 'filed'
           AND rr.status IN ('resolved_upheld', 'resolved_regraded')
           AND rr.result_sent_at IS NULL) = sqlc.narg(only_undelivered_result));

-- Per-round regrade methods (0027): one method per email turn, preset on the
-- Regrade tab, usually a single strict model.
-- name: UpsertRegradeRoundMethod :one
INSERT INTO regrade_round_methods (assessment_id, turn, method_id)
VALUES ($1, $2, $3)
ON CONFLICT (assessment_id, turn) DO UPDATE
    SET method_id = EXCLUDED.method_id, updated_at = now()
RETURNING *;

-- name: ListRegradeRoundMethods :many
SELECT * FROM regrade_round_methods WHERE assessment_id = $1 ORDER BY turn;

-- name: GetRegradeRoundMethod :one
SELECT * FROM regrade_round_methods WHERE assessment_id = $1 AND turn = $2;

-- RegradeRoundCounts feeds the Regrade tab's per-round work summary.
-- name: RegradeRoundCounts :many
SELECT rr.turn,
    count(*) FILTER (WHERE rp.ai_record_id IS NULL AND rp.verdict IS NULL
        AND rr.status IN ('received', 'under_review'))          AS pending,
    count(*) FILTER (WHERE rp.ai_record_id IS NOT NULL AND rp.verdict IS NULL) AS graded,
    count(*) FILTER (WHERE rp.verdict IS NOT NULL)              AS adjudicated
FROM regrade_requests rr
JOIN regrade_request_problems rp ON rp.request_id = rr.id
WHERE rr.assessment_id = $1 AND rr.kind = 'filed' AND rr.turn IS NOT NULL
GROUP BY rr.turn
ORDER BY rr.turn;

-- RoundHasGradedSubItems gates round-method edits: a round's method is preset
-- and editable only until it has actually graded something.
-- name: RoundHasGradedSubItems :one
SELECT EXISTS (
    SELECT 1
    FROM regrade_request_problems rp
    JOIN regrade_requests rr ON rr.id = rp.request_id
    WHERE rr.assessment_id = $1 AND rr.turn = $2 AND rp.ai_record_id IS NOT NULL
) AS graded;

-- PendingRoundSubItems is the round batch-grade work list: filed, still-open
-- requests of this turn whose sub-items have no AI record and no terminal AI
-- error yet.
-- name: PendingRoundSubItems :many
SELECT rp.id
FROM regrade_request_problems rp
JOIN regrade_requests rr ON rr.id = rp.request_id
WHERE rr.assessment_id = $1
  AND rr.turn = $2
  AND rr.kind = 'filed'
  AND rr.status IN ('received', 'under_review')
  AND rp.ai_record_id IS NULL
  AND rp.verdict IS NULL
ORDER BY rp.id;

-- SetSubItemAdoptedRecord stamps the overlay layer for a regraded verdict
-- (0028): the adopted record IS that turn's grade for the answer.
-- name: SetSubItemAdoptedRecord :one
UPDATE regrade_request_problems
SET adopted_record_id = sqlc.narg(record_id)
WHERE id = $1
RETURNING *;

-- SetProblemVerdictAndAdoption records a TA's adjudication AND its overlay in ONE
-- atomic statement (regrade-round correctness fix): the old handler ran SetProblemVerdict
-- then SetSubItemAdoptedRecord as two writes, so a failure between them could strand a
-- verdict='upheld' row still carrying a stale adopted_record_id — an override overlay
-- consumers would apply for a grade the student was never told about. Setting verdict,
-- note, who, when, and adopted_record_id together makes the pair all-or-nothing:
-- adopted_record_id carries the record for a 'regraded' verdict, or SQL NULL (narg) for
-- 'upheld'.
-- name: SetProblemVerdictAndAdoption :one
UPDATE regrade_request_problems
SET verdict = $2, verdict_note = $3, verdict_by = $4, verdict_at = now(),
    adopted_record_id = sqlc.narg(adopted_record_id)
WHERE id = $1
RETURNING *;

-- RegradeLayersForAnswer lists an answer's ADJUDICATED regrade overlays (the
-- layer stack above round 0), oldest turn first — the answer page's layer
-- pager renders one entry per turn that actually touched this problem.
-- name: RegradeLayersForAnswer :many
SELECT rr.turn, rr.id AS request_id, rp.id AS sub_item_id,
    rp.verdict, rp.verdict_note, rp.verdict_at,
    rp.adopted_record_id, gr.total AS adopted_total
FROM regrade_request_problems rp
JOIN regrade_requests rr ON rr.id = rp.request_id
JOIN answers a ON a.assessment_id = rr.assessment_id
    AND a.student_id = rr.student_id AND a.problem_id = rp.problem_id
LEFT JOIN grading_records gr ON gr.id = rp.adopted_record_id
WHERE a.id = $1 AND rp.verdict IS NOT NULL
ORDER BY rr.turn, rp.id;

-- AdoptedOverlaysForAssessment returns, per answer, the EFFECTIVE adopted record —
-- the regrade overlay stack collapsed to its top. Resolution matches ExportRows'
-- canonical LATERAL (regrade-round correctness fix): live publish chain first, then
-- most-recently adjudicated, then turn/id; and only 'regraded' verdicts' adoptions
-- count. Joined by consumers (totals/export) as the effective-grade override of round 0.
-- name: AdoptedOverlaysForAssessment :many
SELECT DISTINCT ON (a.id)
    a.id AS answer_id, rp.adopted_record_id, rr.turn, gr.total
FROM regrade_request_problems rp
JOIN regrade_requests rr ON rr.id = rp.request_id
LEFT JOIN publish_items pi ON pi.id = rr.publish_item_id
LEFT JOIN publish_batches pb ON pb.id = pi.batch_id
JOIN answers a ON a.assessment_id = rr.assessment_id
    AND a.student_id = rr.student_id AND a.problem_id = rp.problem_id
JOIN grading_records gr ON gr.id = rp.adopted_record_id
WHERE rr.assessment_id = $1 AND rp.verdict = 'regraded' AND rp.adopted_record_id IS NOT NULL
ORDER BY a.id, (pb.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC;

-- name: ResolveRegradeRequest :one
-- F2 (TOCTOU on resolve, unchanged from v1): the status guard is enforced HERE,
-- atomically with the write, not just in the handler's pre-check read.
UPDATE regrade_requests
SET status = $2, resolver_id = $3, resolution_note = $4, resolved_at = now()
WHERE id = $1 AND status IN ('received', 'under_review')
RETURNING *;

-- name: SetRegradeStatus :one
-- Used for the intermediate 'under_review' transition.
UPDATE regrade_requests SET status = $2 WHERE id = $1 RETURNING *;

-- name: SetRegradeResultSentAt :one
-- Send-failure recovery path (whole-branch review F1, migration 0026): records that
-- result email #N was actually DELIVERED for this request -- set only after a
-- successful provider send, by both the original send-result flow and the
-- resend-result recovery route. NULL means "resolved but nothing was ever
-- delivered" (a provider failure after the atomic resolve flip) -- the signal
-- resend-result's guard checks for.
UPDATE regrade_requests SET result_sent_at = now() WHERE id = $1 RETURNING *;

-- name: MarkRequestHandedOff :one
-- Consuming the final-turn (handoff) token (spec §6 D60): the request itself records
-- kind='handed_off' (distinct from a normal 'filed' request -- no further adjudication
-- happens on it, the assigned TAs take over person-to-person). Does not touch status;
-- the request's status lifecycle (received/under_review/resolved_*) is orthogonal to
-- kind and callers manage it separately if at all for a handed-off row.
UPDATE regrade_requests SET kind = 'handed_off' WHERE id = $1 RETURNING *;

-- name: InsertRequestProblem :one
-- One sub-item per contested problem (spec §5 D59). UNIQUE(request_id, problem_id)
-- guards the TA escape-hatch add/correct path (§5) from creating a second sub-item
-- for a problem already on the request.
INSERT INTO regrade_request_problems (request_id, problem_id, complaint_text)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListRequestProblems :many
SELECT * FROM regrade_request_problems WHERE request_id = $1 ORDER BY problem_id;

-- name: GetRequestProblem :one
SELECT * FROM regrade_request_problems WHERE id = $1;

-- name: SetProblemVerdict :one
-- TA adjudication of one sub-item (spec §5): outcome upheld|regraded, note, who, when.
-- The CHECK on verdict (migration 0025) rejects anything else at the database level.
UPDATE regrade_request_problems
SET verdict = $2, verdict_note = $3, verdict_by = $4, verdict_at = now()
WHERE id = $1
RETURNING *;

-- name: SetProblemAIRecord :one
-- Links the per-sub-item AI re-grade result (spec §5: AI assist re-scopes to one
-- sub-item per job). Mirrors v1's SetRegradeAIRecord posture: never touches verdict,
-- clears any stale ai_error so a successful re-grade doesn't leave an old failure
-- reason visible next to the freshly linked record.
UPDATE regrade_request_problems SET ai_record_id = $2, ai_error = NULL WHERE id = $1 RETURNING *;

-- name: SetProblemAIError :one
-- Terminal AI re-grade failure reason for one sub-item: short constant string only
-- (CLAUDE.md PII rule), never touches verdict or ai_record_id.
UPDATE regrade_request_problems SET ai_error = $2 WHERE id = $1 RETURNING *;

-- name: CountUnverdictedProblems :one
-- Backs AllProblemsVerdicted(requestID): the send-result gate (spec §5) must 409 until
-- EVERY sub-item on the request has a verdict. Uses the partial index
-- regrade_request_problems_unverdicted_idx (migration 0025, WHERE verdict IS NULL).
SELECT count(*) FROM regrade_request_problems WHERE request_id = $1 AND verdict IS NULL;

-- name: CountRequestProblems :one
-- Companion to CountUnverdictedProblems: a request with ZERO sub-items is not
-- "all verdicted" in any meaningful sense (nothing to send a result about) -- the
-- store-level AllProblemsVerdicted helper combines both counts so that edge case is
-- explicit rather than accidentally true (0 unverdicted out of 0 total).
SELECT count(*) FROM regrade_request_problems WHERE request_id = $1;

-- name: AssignProblemTA :one
-- At most one TA per problem (UNIQUE(problem_id), migration 0025, spec §6 D60):
-- assigning a new TA to an already-assigned problem must replace the row, not error
-- -- the picker UI is "set the TA for this problem", not "add another assignment".
INSERT INTO problem_ta_assignments (problem_id, user_id, assigned_by)
VALUES ($1, $2, $3)
ON CONFLICT (problem_id) DO UPDATE
    SET user_id = EXCLUDED.user_id, assigned_by = EXCLUDED.assigned_by, assigned_at = now()
RETURNING *;

-- name: RemoveProblemTA :exec
-- Unassign (spec §6 D60 UI: "assign/unassign TA") -- deletes the row rather than
-- nulling user_id, since user_id is NOT NULL (an assignment row only exists when
-- there IS an assignee; "no TA" is "no row", matching ListTAAssignments' LEFT JOIN).
DELETE FROM problem_ta_assignments WHERE problem_id = $1;

-- name: GetProblemTA :one
SELECT * FROM problem_ta_assignments WHERE problem_id = $1;

-- name: ListTAAssignments :many
-- Every problem in an assessment with its TA assignment, if any (spec §6: "assignment
-- state visible on regrade rows", publish preview's unassigned-problem warning). LEFT
-- JOIN so unassigned problems still appear (assigned columns NULL).
SELECT p.id AS problem_id, p.number AS problem_number,
    pta.user_id, pta.assigned_by, pta.assigned_at
FROM problems p
LEFT JOIN problem_ta_assignments pta ON pta.problem_id = p.id
WHERE p.assessment_id = $1
ORDER BY p.position, p.number;

-- name: InsertRegradeAIRecord :one
-- The stricter AI re-grade result (spec §8 v1, D50 -- unaffected by the v2 schema
-- change: this inserts into grading_records, not regrade_requests, so migration 0025
-- doesn't touch it). An append-only grading_records row with source='regrade_ai' and
-- a baked-in policy (regrade_strict). run_id is NULL (this is a single-leaf, on-demand
-- job, not a run leaf) and there is no created_by (machine-authored -- migration 0024
-- widens grading_records_check to admit that). model_id / method / rubric / refsol /
-- prompt are pinned from the CONTESTED official record so the comparison stays
-- apples-to-apples (D53). NEVER auto-official: the caller links it via
-- SetProblemAIRecord (v2: one grading_records row per sub-item, spec §5) and the TA
-- walks the normal path.
INSERT INTO grading_records (
    answer_id, source, provider, model_id, method_version_id,
    rubric_version_id, reference_solution_version_id, prompt_template_version_id,
    graded_image_shas, criterion_scores, total, comment, transcription, confidence,
    adjustments, raw_output, input_tokens, output_tokens, cost_usd, temperature, policy
)
VALUES ($1, 'regrade_ai', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
RETURNING *;

-- name: StudentOfficialTotalForProblem :one
-- A student's current official (score, max) for ONE problem, straight from live grading
-- state -- the fresh figure the result email announces for a REGRADED verdict (C2:
-- never the superseded snapshot the token was minted against). graded counts whether
-- the answer carries an official record; zero means nothing is official for this
-- (student, problem) yet, so the caller omits the New-score line rather than announcing
-- "0/max".
SELECT
    coalesce(sum(gr.total), 0)::numeric AS total,
    coalesce((SELECT p.max_points FROM problems p WHERE p.id = $2), 0)::numeric AS max,
    count(a.official_record_id) AS graded
FROM answers a
LEFT JOIN grading_records gr ON gr.id = a.official_record_id
WHERE a.student_id = $1 AND a.problem_id = $2;

-- name: PriorProblemVerdicts :many
-- The verdict history for one problem across every FILED request of a publish item's
-- token chain (spec §6: the assigned TA gets the student's full prior-turn history for
-- that problem on handoff). Joins each filed request's sub-item for the given problem
-- NUMBER (matched via problems.number so it survives the handoff request's own row), in
-- turn order. Verdict may be NULL for a still-open prior turn; the caller skips those.
SELECT rr.turn AS turn, rrp.verdict AS verdict, rrp.verdict_note AS verdict_note
FROM regrade_requests rr
JOIN regrade_request_problems rrp ON rrp.request_id = rr.id
JOIN problems p ON p.id = rrp.problem_id
WHERE rr.publish_item_id = $1
  AND p.number = $2
  AND rr.kind = 'filed'
ORDER BY rr.turn;

-- name: ContestedAnswerForSubItem :many
-- The single contested answer a sub-item's AI re-grade re-examines (spec §8, per-sub-item
-- re-scope): the student's answer for THAT problem, with the CURRENT EFFECTIVE record's
-- pinned method/rubric/refsol/prompt and scores so the re-grade is apples-to-apples
-- (D53). Returns 0 or 1 row (a student has at most one answer per problem); :many keeps
-- the caller's "empty ⇒ nothing to re-examine, fail open" shape.
--
-- EFFECTIVE, not round-0 official (regrade-round correctness fix): a turn-N re-grade
-- must be briefed against the grade the student is actually contesting — round 0
-- overlaid by the latest ADOPTED record from a prior turn — not round 0 itself, or a
-- turn-2 re-grade would silently re-examine (and could lower) a grade turn 1 already
-- raised. The overlay LATERAL matches ExportRows' canonical resolver, plus an exclude
-- clause: the sub-item being adjudicated NOW is skipped so it never briefs against its
-- own (possibly re-run) prior adoption. The effective record is then joined on
-- COALESCE(overlay.record_id, a.official_record_id); no effective record ⇒ no row.
SELECT a.id AS answer_id, a.problem_id,
    egr.id AS record_id, egr.provider, egr.model_id, egr.method_version_id,
    egr.rubric_version_id, egr.reference_solution_version_id, egr.prompt_template_version_id,
    egr.criterion_scores, egr.comment AS original_comment
FROM answers a
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
      AND rp.id <> sqlc.arg(exclude_sub_item_id)
    ORDER BY (pb.superseded_at IS NOT NULL), rp.verdict_at DESC NULLS LAST, rr.turn DESC, rp.id DESC
    LIMIT 1
) overlay ON true
JOIN grading_records egr ON egr.id = COALESCE(overlay.record_id, a.official_record_id)
WHERE a.assessment_id = sqlc.arg(assessment_id)
  AND a.student_id = sqlc.arg(student_id)
  AND a.problem_id = sqlc.arg(problem_id)
ORDER BY a.problem_id;

-- name: ListEligibleAIRegradeSubItems :many
-- The "AI re-grade all" enqueue set, re-scoped to SUB-ITEMS (spec §8): every contested
-- problem of an OPEN FILED request in the assessment that has no AI record yet, paired
-- with the contested official record's provider/model for the cost estimate. A sub-item
-- with no official answer to re-examine is still enumerable but drops out of the pricing
-- (LEFT JOIN leaves provider/model NULL) -- it fails open at execution time with a
-- terminal ai_error, same posture as the single-sub-item path.
SELECT rrp.id AS sub_item_id, rrp.problem_id,
    gr.provider AS provider, gr.model_id AS model_id
FROM regrade_request_problems rrp
JOIN regrade_requests rr ON rr.id = rrp.request_id
LEFT JOIN answers a ON a.assessment_id = rr.assessment_id AND a.student_id = rr.student_id
    AND a.problem_id = rrp.problem_id
LEFT JOIN grading_records gr ON gr.id = a.official_record_id
WHERE rr.assessment_id = $1
  AND rr.kind = 'filed'
  AND rr.status IN ('received', 'under_review')
  AND rrp.ai_record_id IS NULL
ORDER BY rrp.id;

-- name: CountSkippedAIRegradeSubItems :one
-- Sub-items the "AI re-grade all" batch skips (spec §8): those on open filed requests
-- that ALREADY carry an AI record. Reported alongside the enqueued count so a TA sees
-- why fewer jobs ran than there are contested problems.
SELECT count(*)
FROM regrade_request_problems rrp
JOIN regrade_requests rr ON rr.id = rrp.request_id
WHERE rr.assessment_id = $1
  AND rr.kind = 'filed'
  AND rr.status IN ('received', 'under_review')
  AND rrp.ai_record_id IS NOT NULL;
