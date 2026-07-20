-- Analysis/report queries: each assessment is a dataset for comparing grading
-- methods (plan §8 objective "measure & improve"; B-H14/B-M19 seeds).

-- name: MethodStatsForAssessment :many
SELECT p.id AS problem_id, p.number AS problem_number, p.max_points,
    mv.id AS method_version_id, mv.method_id, m.name AS method_name, mv.version AS method_version,
    -- Keep the COALESCE: pre-D25 configs have no 'policy' key, and a bare ->>
    -- yields SQL NULL that fails sqlc's non-nullable string scan (500s the
    -- analysis endpoint). TestAnalysis_StatsAndAgreement exercises this path.
    COALESCE(mv.config->>'policy', '')::text AS policy,
    count(*) AS records,
    avg(gr.total)::numeric(8, 2) AS mean_total,
    (percentile_cont(0.5) WITHIN GROUP (ORDER BY gr.total::float8))::numeric(8, 2) AS median_total,
    COALESCE(stddev_samp(gr.total), 0)::numeric(8, 2) AS stddev_total,
    count(*) FILTER (WHERE gr.total = 0) AS zeros,
    count(*) FILTER (WHERE gr.total = p.max_points) AS maxes,
    count(*) FILTER (WHERE gr.confidence = 'high') AS conf_high,
    count(*) FILTER (WHERE gr.confidence = 'medium') AS conf_medium,
    count(*) FILTER (WHERE gr.confidence = 'low') AS conf_low,
    count(*) FILTER (WHERE gr.confidence = 'illegible') AS conf_illegible,
    COALESCE(sum(gr.input_tokens), 0)::bigint AS input_tokens,
    COALESCE(sum(gr.output_tokens), 0)::bigint AS output_tokens
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
JOIN problems p ON p.id = a.problem_id
JOIN grading_method_versions mv ON mv.id = gr.method_version_id
JOIN grading_methods m ON m.id = mv.method_id
WHERE a.assessment_id = $1 AND gr.source = 'model'
GROUP BY p.id, p.number, p.max_points, mv.id, mv.method_id, m.name, mv.version
ORDER BY p.number, m.name, mv.version;

-- Method disagreement (analysis redesign plan, Task B1): per-answer spread of
-- totals across method-versions, mirroring the HumanAgreementForAssessment
-- semantics EXACTLY so adjacent numbers on the Analysis tab never contradict
-- each other — latest source='model' record per (answer, method-version)
-- (gr.id DESC, same "latest" as latest_human), scored records only
-- (total IS NOT NULL), and the problem's CURRENT rubric version only (totals
-- graded against different rubrics are not comparable). Answers with fewer than
-- two method-versions' records have no spread and are excluded, so a
-- single-method assessment yields zero rows (the frontend's hide signal).

-- name: DisagreementByProblem :many
WITH current_rubric AS (
    SELECT DISTINCT ON (rv.problem_id) rv.problem_id, rv.id
    FROM rubric_versions rv
    ORDER BY rv.problem_id, rv.version DESC
),

latest_model AS (
    SELECT DISTINCT ON (gr.answer_id, gr.method_version_id)
        gr.answer_id, a.problem_id, gr.total
    FROM grading_records gr
    JOIN answers a ON a.id = gr.answer_id
    JOIN current_rubric cr ON cr.problem_id = a.problem_id AND cr.id = gr.rubric_version_id
    WHERE a.assessment_id = $1 AND gr.source = 'model' AND gr.total IS NOT NULL
    ORDER BY gr.answer_id, gr.method_version_id, gr.id DESC
),

spreads AS (
    SELECT lm.problem_id, lm.answer_id, max(lm.total) - min(lm.total) AS spread
    FROM latest_model lm
    GROUP BY lm.problem_id, lm.answer_id
    HAVING count(*) >= 2
)

SELECT p.id AS problem_id, p.number AS problem_number, p.max_points,
    count(*) AS answers_compared,
    (percentile_cont(0.5) WITHIN GROUP (ORDER BY s.spread::float8))::numeric(8, 2) AS median_spread,
    -- "Big gap" = spread of at least a point, or 10% of the problem, whichever
    -- is larger — the same threshold the frontend's "methods split" flag uses.
    count(*) FILTER (WHERE s.spread >= GREATEST(1, 0.1 * p.max_points)) AS big_gap_count
FROM spreads s
JOIN problems p ON p.id = s.problem_id
GROUP BY p.id, p.number, p.max_points
ORDER BY p.number;

-- name: DisagreementTopAnswers :many
-- The 10 widest per-answer gaps, one row per (answer, method-version) — Go
-- groups consecutive rows into each answer's scores array. Same CTE semantics
-- as DisagreementByProblem above (keep the two in lockstep). student_id is the
-- roster's EXTERNAL id (display handle, never the name — PII discipline).
WITH current_rubric AS (
    SELECT DISTINCT ON (rv.problem_id) rv.problem_id, rv.id
    FROM rubric_versions rv
    ORDER BY rv.problem_id, rv.version DESC
),

latest_model AS (
    SELECT DISTINCT ON (gr.answer_id, gr.method_version_id)
        gr.answer_id, a.problem_id, gr.method_version_id, gr.total
    FROM grading_records gr
    JOIN answers a ON a.id = gr.answer_id
    JOIN current_rubric cr ON cr.problem_id = a.problem_id AND cr.id = gr.rubric_version_id
    WHERE a.assessment_id = $1 AND gr.source = 'model' AND gr.total IS NOT NULL
    ORDER BY gr.answer_id, gr.method_version_id, gr.id DESC
),

top_spreads AS (
    SELECT lm.answer_id, (max(lm.total) - min(lm.total))::numeric(8, 2) AS spread
    FROM latest_model lm
    GROUP BY lm.answer_id
    HAVING count(*) >= 2
    ORDER BY spread DESC, lm.answer_id
    LIMIT 10
)

SELECT ts.answer_id, ts.spread,
    st.student_id AS student_display, p.number AS problem_number,
    lm.method_version_id, m.name AS method_name, mv.version AS method_version, lm.total
FROM top_spreads ts
JOIN latest_model lm ON lm.answer_id = ts.answer_id
JOIN answers a ON a.id = ts.answer_id
JOIN problems p ON p.id = a.problem_id
JOIN students st ON st.id = a.student_id
JOIN grading_method_versions mv ON mv.id = lm.method_version_id
JOIN grading_methods m ON m.id = mv.method_id
ORDER BY ts.spread DESC, ts.answer_id, m.name, mv.version;

-- name: HumanAgreementForAssessment :many
WITH latest_human AS (
    SELECT DISTINCT ON (gr.answer_id) gr.answer_id, gr.total, gr.rubric_version_id
    FROM grading_records gr
    JOIN answers a ON a.id = gr.answer_id
    WHERE a.assessment_id = $1 AND gr.source = 'human' AND gr.total IS NOT NULL
    ORDER BY gr.answer_id, gr.id DESC
)
SELECT p.number AS problem_number,
    mv.id AS method_version_id, m.name AS method_name, mv.version AS method_version,
    count(*) AS pairs,
    avg(abs(gr.total - h.total))::numeric(8, 2) AS mean_abs_diff,
    count(*) FILTER (WHERE gr.total = h.total) AS exact_matches,
    count(*) FILTER (WHERE abs(gr.total - h.total) <= 1) AS within_one
FROM grading_records gr
JOIN latest_human h ON h.answer_id = gr.answer_id AND h.rubric_version_id = gr.rubric_version_id
JOIN answers a ON a.id = gr.answer_id
JOIN problems p ON p.id = a.problem_id
JOIN grading_method_versions mv ON mv.id = gr.method_version_id
JOIN grading_methods m ON m.id = mv.method_id
WHERE gr.source = 'model' AND gr.total IS NOT NULL
GROUP BY p.number, mv.id, m.name, mv.version
ORDER BY p.number, m.name, mv.version;

-- name: PolicyMixForAssessment :many
-- Problems whose OFFICIAL records were graded under more than one distinct grading
-- policy (D25) — a signal that the official grade may be inconsistent across
-- students for the same problem, e.g. some accepted under lenient and others strict.
SELECT p.id AS problem_id, p.number AS problem_number,
    array_agg(DISTINCT gr.policy ORDER BY gr.policy)::text[] AS policies
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN grading_records gr ON gr.id = a.official_record_id
WHERE p.assessment_id = $1 AND gr.policy IS NOT NULL
GROUP BY p.id, p.number
HAVING count(DISTINCT gr.policy) > 1
ORDER BY p.number;

-- Score distribution (trust spec §5, B-H14, D38): "Problem 2 is all zeros" must be
-- visible to a human before publish. Two independent source sets feed the same shape
-- of stats — OFFICIAL grades when any exist, or (fallback, D38) the single latest run
-- that graded this problem when officials are sparse. handleScoreDistribution in Go
-- decides which source to use by first checking CountOfficialForProblem; the two
-- *ForProblem query pairs below read from each source respectively so the SQL itself
-- never has to branch.

-- name: CountOfficialForProblem :one
-- Denominator/numerator pair for the sparse-officials decision: how many of this
-- problem's answers have an official grade set at all.
SELECT
    count(*) FILTER (WHERE a.official_record_id IS NOT NULL) AS officials,
    count(*) AS total
FROM answers a
WHERE a.problem_id = $1;

-- name: LatestRunForProblem :one
-- The most recently created run that produced at least one model grading_record for
-- this problem's answers — the fallback source when officials are sparse (D38).
SELECT gr.run_id AS run_id
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
WHERE a.problem_id = $1 AND gr.source = 'model' AND gr.run_id IS NOT NULL
ORDER BY gr.run_id DESC
LIMIT 1;

-- name: DistributionTotalsOfficial :one
-- Total-score mean/stddev/%zero/%max over this problem's OFFICIAL grades. p.max_points
-- is echoed back so the caller can compute %zero/%max without a second round trip.
SELECT p.max_points,
    count(*) AS n,
    avg(gr.total)::numeric(8, 4) AS mean_total,
    COALESCE(stddev_samp(gr.total), 0)::numeric(8, 4) AS stddev_total,
    count(*) FILTER (WHERE gr.total = 0) AS zeros,
    count(*) FILTER (WHERE gr.total = p.max_points) AS maxes
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN grading_records gr ON gr.id = a.official_record_id
WHERE a.problem_id = $1 AND gr.total IS NOT NULL
GROUP BY p.max_points;

-- name: DistributionTotalsForRun :one
-- Same shape as DistributionTotalsOfficial, but sourced from one run's model records
-- (the sparse-officials fallback, D38).
SELECT p.max_points,
    count(*) AS n,
    avg(gr.total)::numeric(8, 4) AS mean_total,
    COALESCE(stddev_samp(gr.total), 0)::numeric(8, 4) AS stddev_total,
    count(*) FILTER (WHERE gr.total = 0) AS zeros,
    count(*) FILTER (WHERE gr.total = p.max_points) AS maxes
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN grading_records gr ON gr.answer_id = a.id AND gr.run_id = $2
WHERE a.problem_id = $1 AND gr.source = 'model' AND gr.total IS NOT NULL
GROUP BY p.max_points;

-- name: DistributionHistogramOfficial :many
-- 10-bucket histogram of OFFICIAL totals as a fraction of max_points (bucket 1 =
-- [0%,10%), ..., bucket 10 = [90%,100%], top bucket inclusive of the max — LEAST(...,
-- 10) folds width_bucket's out-of-range 11th bucket, which it returns exactly at
-- x = high, back into bucket 10). width_bucket needs a strictly-positive range, so a
-- problem with max_points = 0 is excluded here and handled as a zero-histogram case
-- in Go. Buckets are 1-indexed here; the Go layer shifts to a 0-indexed [10]int64.
SELECT LEAST(width_bucket(gr.total::float8, 0, p.max_points::float8, 10), 10)::int AS bucket, count(*) AS n
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN grading_records gr ON gr.id = a.official_record_id
WHERE a.problem_id = $1 AND gr.total IS NOT NULL AND p.max_points > 0
GROUP BY bucket
ORDER BY bucket;

-- name: DistributionHistogramForRun :many
-- Same as DistributionHistogramOfficial, sourced from one run's model records.
SELECT LEAST(width_bucket(gr.total::float8, 0, p.max_points::float8, 10), 10)::int AS bucket, count(*) AS n
FROM answers a
JOIN problems p ON p.id = a.problem_id
JOIN grading_records gr ON gr.answer_id = a.id AND gr.run_id = $2
WHERE a.problem_id = $1 AND gr.source = 'model' AND gr.total IS NOT NULL AND p.max_points > 0
GROUP BY bucket
ORDER BY bucket;

-- name: DistributionCriteriaOfficial :many
-- Per-criterion mean/stddev/%zero/%max over OFFICIAL grades, unnesting the
-- criterion_scores JSONB array ([{criterion_id, score, rationale}], D4) per record.
-- rc.points is joined in so %max/%zero use the CURRENT rubric's per-criterion max —
-- fine because handleScoreDistribution only ever calls this for the latest rubric
-- version's criteria (records graded against an older version simply won't match
-- rv.id and are silently excluded, same "rubric_version scoping" convention as
-- HumanAgreementForAssessment above).
SELECT rc.id AS criterion_id, rc.description, rc.points,
    count(*) AS n,
    avg((cs.value->>'score')::numeric)::numeric(8, 4) AS mean_score,
    COALESCE(stddev_samp((cs.value->>'score')::numeric), 0)::numeric(8, 4) AS stddev_score,
    count(*) FILTER (WHERE (cs.value->>'score')::numeric = 0) AS zeros,
    count(*) FILTER (WHERE (cs.value->>'score')::numeric = rc.points) AS maxes
FROM answers a
JOIN grading_records gr ON gr.id = a.official_record_id
JOIN rubric_criteria rc ON rc.rubric_version_id = gr.rubric_version_id
JOIN LATERAL jsonb_array_elements(gr.criterion_scores) AS cs(value)
    ON (cs.value->>'criterion_id')::bigint = rc.id
WHERE a.problem_id = $1
GROUP BY rc.id, rc.description, rc.points, rc.position
ORDER BY rc.position;

-- name: DistributionCriteriaForRun :many
-- Same as DistributionCriteriaOfficial, sourced from one run's model records.
SELECT rc.id AS criterion_id, rc.description, rc.points,
    count(*) AS n,
    avg((cs.value->>'score')::numeric)::numeric(8, 4) AS mean_score,
    COALESCE(stddev_samp((cs.value->>'score')::numeric), 0)::numeric(8, 4) AS stddev_score,
    count(*) FILTER (WHERE (cs.value->>'score')::numeric = 0) AS zeros,
    count(*) FILTER (WHERE (cs.value->>'score')::numeric = rc.points) AS maxes
FROM answers a
JOIN grading_records gr ON gr.answer_id = a.id AND gr.run_id = $2 AND gr.source = 'model'
JOIN rubric_criteria rc ON rc.rubric_version_id = gr.rubric_version_id
JOIN LATERAL jsonb_array_elements(gr.criterion_scores) AS cs(value)
    ON (cs.value->>'criterion_id')::bigint = rc.id
WHERE a.problem_id = $1
GROUP BY rc.id, rc.description, rc.points, rc.position
ORDER BY rc.position;

-- name: OverrideRateByMethod :many
-- Override rate per method (trust spec §7, D40): for every method_version that
-- produced a model record on this assessment, the share of that method's answers
-- where the OFFICIAL record ended up being a human record — i.e. a human
-- replaced-or-adjusted that method's AI suggestion — plus the mean |Δ| between the
-- method's AI total and the final official total (both directions: a human "agreeing"
-- exactly still counts in the denominator, since it's still officially a human
-- record; the point is trust in the AI's raw output, not just disagreement rate).
-- Only answers where this method has a model record AND an official record exists
-- are counted (an answer nobody has set official for yet says nothing about override
-- behavior either way).
WITH method_ai AS (
    SELECT DISTINCT ON (gr.answer_id, mv.id)
        gr.answer_id, mv.id AS method_version_id, m.name AS method_name, mv.version AS method_version,
        gr.total AS ai_total
    FROM grading_records gr
    JOIN answers a ON a.id = gr.answer_id
    JOIN grading_method_versions mv ON mv.id = gr.method_version_id
    JOIN grading_methods m ON m.id = mv.method_id
    WHERE a.assessment_id = $1 AND gr.source = 'model'
    ORDER BY gr.answer_id, mv.id, gr.id DESC
)
SELECT ma.method_version_id, ma.method_name, ma.method_version,
    count(*) AS answers,
    count(*) FILTER (WHERE off.source = 'human') AS human_overrides,
    -- Split the human-official answers by whether the AI actually produced a
    -- score: an abstention (illegible ⇒ ai_total NULL) that a human filled in is
    -- NOT the AI being wrong, so counting it as an "override" overstates
    -- disagreement. scored_disagreements = human replaced a real AI score;
    -- filled_blanks = human filled a cell the AI declined to grade.
    count(*) FILTER (WHERE off.source = 'human' AND ma.ai_total IS NOT NULL) AS scored_disagreements,
    count(*) FILTER (WHERE off.source = 'human' AND ma.ai_total IS NULL) AS filled_blanks,
    -- Exact decimal division in SQL (never float64, matching the money/points
    -- convention for every other rate/stat this endpoint returns).
    (count(*) FILTER (WHERE off.source = 'human')::numeric / count(*))::numeric(8, 4) AS override_rate,
    COALESCE(avg(abs(ma.ai_total - off.total)) FILTER (WHERE ma.ai_total IS NOT NULL AND off.total IS NOT NULL), 0)::numeric(8, 4) AS mean_abs_diff
FROM method_ai ma
JOIN answers a ON a.id = ma.answer_id
JOIN grading_records off ON off.id = a.official_record_id
GROUP BY ma.method_version_id, ma.method_name, ma.method_version
ORDER BY ma.method_name, ma.method_version;
