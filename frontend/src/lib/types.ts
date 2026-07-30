// JSON shapes of the Phase-1 API (mirrors internal/httpapi/*.go).
//
// All point values (max_points, criterion points, score_increment) are decimal
// STRINGS like "10" or "2.5" — never parse them into floats (docs/DECISIONS.md D4).
// Use src/lib/decimal.ts for arithmetic.

export interface Assessment {
  id: number;
  kind: string; // "exam" | "assignment"
  name: string;
  archived: boolean;
  problem_count?: number;
  created_at?: string;
  /** Round-based grading (0027): the exam-wide final grading source. Absent =
   * not chosen yet (nothing is official until it is). */
  final_source_kind?: "method" | "consensus";
  final_method_id?: number;
  /** Exact completed grading run selected when final_source_kind is method. */
  final_run_id?: number;
  /** Regrade cutoff — replies after this are recorded but rejected. */
  regrade_deadline?: string;
}

export interface Problem {
  id: number;
  assessment_id: number;
  number: number;
  title: string;
  statement: string;
  max_points: string; // decimal string — keep as string
  position: number;
}

export interface AssessmentDetailResponse {
  assessment: Assessment;
  problems: Problem[];
}

export interface RubricCriterion {
  id: number;
  position: number;
  description: string;
  points: string; // decimal string
  partial_credit_notes: string;
}

export interface RubricVersion {
  id: number;
  version: number;
  notes: string;
  score_increment: string; // decimal string
  created_at?: string;
  criteria?: RubricCriterion[]; // only on `current` and single-version fetches
}

export interface RubricResponse {
  current: RubricVersion | null;
  versions: RubricVersion[];
}

export interface SolutionVersion {
  id: number;
  version: number;
  content?: string; // only on `current` and single-version fetches
  created_at?: string;
}

export interface SolutionsResponse {
  current: SolutionVersion | null;
  versions: SolutionVersion[];
}

export interface Student {
  id: number;
  student_id: string;
  name: string;
  email: string;
  withdrawn?: boolean;
}

/** DELETE /api/students/{id} 200 response (B15, admin-only hard delete). */
export interface StudentDeleteResult {
  id: number;
  deleted: boolean;
}

/** DELETE /api/students/{id} 409 body when real artifacts exist (B15): `blocking` lists
 * only the kinds actually present, in a fixed server-side order — see
 * lib/labels.ts studentBlockingKindLabel for the human copy. Withdraw is the reversible
 * alternative in every one of these cases. */
export interface StudentDeleteBlockedError {
  error: string;
  blocking: string[];
}

/**
 * Roster import diff (roster-lifecycle plan 2026-07-10, shared contract): what the
 * uploaded CSV implies about the existing roster WITHOUT mutating anything — the
 * Students page proposes bulk withdraw / reinstate from it; sync is never automatic.
 * The id lists carry student_ids only; email/name changes are counts only (PII — the
 * values never leave the server). Arrays may be null (Go nil slice) — default `?? []`.
 */
export interface RosterDiff {
  /** Active student_ids in the DB that are absent from the CSV (add/drop, 停修). */
  missing_active: string[] | null;
  /** Withdrawn student_ids present in the CSV (retaker trap — reinstate explicitly). */
  withdrawn_present: string[] | null;
  email_changed: number;
  name_changed: number;
}

/** POST /api/students/import response (roster.Report). The roster-lifecycle diff is
 * embedded additively at the top level (existing fields unchanged); optional so a
 * pre-diff payload still typechecks. */
export interface ImportReport extends Partial<RosterDiff> {
  added: number;
  updated: number;
  unchanged: number;
  total: number;
}

/** POST /api/students/bulk-withdraw and /api/students/bulk-reinstate response
 * (lecturer+, body {"student_ids": ["..."]}; unknown ids are a 400 listing them). */
export interface BulkStudentStatusResult {
  updated: number;
}

export interface UserRow {
  id: number;
  email: string;
  display_name: string;
  role: string;
  active: boolean;
}

// --- Phase 2: ingestion + masking (mirrors internal/httpapi/ingestion.go) ----------

/**
 * Quarantine-assign result (POST /api/quarantine/{id}/assign, ingest.FileResult):
 * this path stays synchronous under D27 — only the roster-based bulk upload became
 * async — so its response shape is unchanged.
 */
export interface IngestFileResult {
  filename: string;
  student_id?: string;
  status: "ingested" | "quarantined" | "rejected";
  reason?: string;
  pages?: number;
  mapped_pages?: number;
  submission_id?: number;
}

/**
 * POST /api/assessments/{id}/submissions result row (D27, F1): upload is now a
 * synchronous staging gate followed by off-request ingest, so this reports only
 * "queued" (staged, ingest job enqueued) or "rejected" (the sync gate rejected it —
 * no row, no ingest). Poll GET .../uploads (DirectUploadRow) for the ingest outcome.
 */
export interface DirectUploadResult {
  filename: string;
  upload_id?: number;
  status: "queued" | "rejected";
  reason?: string;
}

/**
 * GET /api/assessments/{id}/uploads row: the ingest-outcome view for one staged
 * upload. "pending" until the ingest job finishes; then "ingested" | "quarantined" |
 * "rejected" | "error" per handleListDirectUploads/directUploadStatus.
 */
export interface DirectUploadRow {
  id: number;
  filename: string;
  status: "pending" | "ingested" | "quarantined" | "rejected" | "error";
  reason?: string;
  submission_id?: number;
  created_at?: string;
}

export interface IngestReportStudent {
  student_id: string;
  name: string;
  submission_id?: number;
  filename?: string;
  page_count?: number;
  mapped_pages: number;
  expected_pages: number;
}

export interface QuarantineEntry {
  id: number;
  filename: string;
  reason: string;
}

export interface IngestReport {
  students: IngestReportStudent[];
  quarantine: QuarantineEntry[];
}

/** GET /api/assessments/{id}/students/{sid}/submission */
export interface StudentSubmissionView {
  student: { student_id: string; name: string; email: string };
  submission: {
    id: number;
    filename: string;
    page_count: number;
    uploaded_at?: string;
  } | null;
  answers: {
    answer_id: number;
    problem_number: number;
    problem_title: string;
    record_count: number;
    has_official: boolean;
    pages: { page_id: number; page_index: number; masked: boolean }[];
  }[];
}

/** Normalized 0..1 page coordinates; floats are fine here (geometry, not points). */
export interface MaskRegion {
  page_scope: "first" | "all";
  x: number;
  y: number;
  w: number;
  h: number;
  color: string;
  padding: number;
}

export interface MaskReviewPage {
  page_id: number;
  answer_id: number;
  page_index: number;
  student_id: string;
  problem_number: number;
  masked: boolean;
  review_status: string; // "pending" | "accepted" | "flagged"
  // Static, PII-free terminal reason set when this page's mask job exhausted its
  // attempts (D27 review, F1). Empty/absent when the page has no error. A page
  // with a mask_error is terminal: the review poll must stop for it, and the UI
  // shows the reason with a hint that "Apply masks" retries it.
  mask_error?: string;
}

/**
 * POST /api/assessments/{id}/masks/apply response (D27, F2): masking now runs as
 * background mask.page jobs rather than synchronously, so the response reports what
 * was planned, not what finished — poll ["mask-review", id] for completion.
 */
export interface ApplyMasksResult {
  enqueued: number; // pages enqueued for (re-)masking this run
  skipped: number; // pages already up to date (regions unchanged since last mask)
}

// --- Phase 3: review + grading (mirrors internal/httpapi/review.go) -----------------

export interface ProblemSummary {
  problem_id: number;
  number: number;
  title: string;
  max_points: string; // decimal string
  answers: number;
  with_pages: number;
  official_set: number;
  ai_graded: number;
  human_graded: number;
  flagged: number;
  published: number;
}

export type AnswerStatus =
  | "no_submission"
  | "ungraded"
  | "graded"
  | "official_set"
  | "published";

export interface ProblemStudentRow {
  answer_id: number;
  student_id: string;
  name: string;
  email: string;
  flags: string[];
  page_count: number;
  record_count: number;
  official_total?: string; // decimal string
  official_source?: string;
  published_at?: string;
  status: AnswerStatus;
}

export interface CriterionScore {
  criterion_id: number;
  score: string; // decimal string
  rationale?: string;
}

export interface ScoreAdjustment {
  criterion_id: number;
  from: string;
  to: string;
}

export interface GradingRecord {
  id: number;
  source: string; // "human" | "model" | "aggregate"
  run_id?: number;
  provider?: string;
  model_id?: string;
  method_version_id?: number;
  method_version?: number; // human version integer behind method_version_id
  prompt_template_version_id?: number;
  prompt_version?: number; // human version integer behind prompt_template_version_id
  policy?: string; // "lenient" | "standard" | "strict" (D25)
  rubric_version_id: number;
  criterion_scores: CriterionScore[];
  total: string | null; // decimal string
  comment: string;
  transcription?: string;
  confidence?: string;
  adjustments: ScoreAdjustment[] | null;
  graded_image_shas: string[];
  created_by?: number;
  created_at?: string;
}

export interface AnswerPage {
  id: number;
  page_index: number;
  submission_id: number;
  pdf_page_index: number;
  width: number;
  height: number;
  masked: boolean;
  mask_review: string; // "pending" | "accepted" | "flagged"
}

export interface AnswerResponse {
  answer: {
    id: number;
    assessment_id: number;
    problem_id: number;
    flags: string[];
    official_record_id: number | null;
    published_at: string | null;
  };
  student: { student_id: string; name: string; email: string };
  problem: { id: number; number: number; title: string; max_points: string; statement: string };
  assessment_name: string;
  pages: AnswerPage[];
  /** Adjudicated regrade overlays stacked on round 0 (rounds design), oldest
   * turn first — drives the answer page's layer pager. */
  regrade_layers: RegradeLayer[];
  records: GradingRecord[];
}

// --- Phase 4: methods + runs (mirrors internal/httpapi/methods.go, runs.go) ---------

/** Config-as-data payload of a method version (internal/grading/method.go). */
export interface MethodConfig {
  provider: string;
  model: string;
  temperature: number;
  reasoning_level?: string; // "off" | "low" | "medium" | "high"
  ref_solutions: number; // 0 or 1 in v0
  reask_cap: number;
  policy?: string; // "lenient" | "standard" | "strict" (D25); empty ⇒ standard
  prompt_template_version_id: number;
  max_tokens?: number;
}

/** GET /api/grading-policies — the curated catalog (internal/grading/prompt.go Policies). */
export interface GradingPolicy {
  key: string; // "lenient" | "standard" | "strict"
  label: string;
  tagline: string;
  when_to_use: string;
}

export interface MethodVersion {
  id: number;
  version: number;
  config: MethodConfig;
  created_at?: string;
}

export interface Method {
  id: number;
  name: string;
  archived: boolean;
  latest?: MethodVersion;
}

export interface MethodDetailResponse {
  method: Method;
  versions: MethodVersion[]; // newest first
}

export interface PromptTemplate {
  id: number;
  name: string;
  version: number;
  system_template: string;
  user_template: string;
}

export type RunStatus =
  | "pending"
  | "running"
  | "paused"
  | "cancelled"
  | "completed"
  | "failed";

export type RunItemState = "pending" | "running" | "succeeded" | "failed" | "skipped";

/** Per-state leaf counts; absent key = 0. */
export type RunCounts = Partial<Record<RunItemState, number>>;

export type ScopeKind = "assessment" | "problem" | "answer" | "sample";

export interface Run {
  id: number;
  assessment_id: number;
  scope_kind: ScopeKind;
  scope_id: number;
  method_version_id: number;
  status: RunStatus;
  error?: string;
  counts: RunCounts;
  created_at?: string;
  started_at?: string;
  finished_at?: string;
  // --- S3: cost cap + reporting (runJSON, trust spec §3/§7, landed a3a6b96) ---
  cost_cap_usd?: string; // decimal string; absent when no cap was set
  cost_usd: string; // decimal string; "0" (never absent) when no priced records yet
  input_tokens: number;
  output_tokens: number;
}

/** GET /api/runs rows carry display names the bare run object lacks. */
export interface RunListRow extends Run {
  method_name: string;
  method_version: number;
  assessment_name: string;
}

export interface RunItem {
  id: number;
  answer_id: number;
  student_id: string;
  problem_number: number;
  model: string;
  state: RunItemState;
  attempts: number;
  error?: string;
  record_id?: number;
}

export interface RunDetailResponse {
  run: Run;
  items: RunItem[];
  /** true when `?all=1` was requested and the server response hit runItemsLimit. */
  truncated: boolean;
  /** echoes whether this response is the full item list (`?all=1`) or the default filtered view. */
  all: boolean;
}

/** One entry of GET /api/runs/preview's `blockers` array (RunBlocker,
 * internal/httpapi/runs.go, B9-backend audit 2026-07-16): a machine-readable reason THIS
 * launch is guaranteed to fail (as opposed to WorkflowWarning's advisory hazards a TA may
 * knowingly launch through) — e.g. a problem in scope has no rubric or no reference
 * solution. `message` is already a complete, human-readable sentence fragment; render it
 * verbatim rather than re-deriving copy from `code`. */
export interface RunBlocker {
  code: string;
  problem_id?: number;
  message: string;
}

export interface RunPreview {
  answers: number;
  mask_blockers: number;
  /** Launch-scoped workflow-guard warnings (plan 2026-07-10, Task B2). */
  warnings?: WorkflowWarning[];
  /** Always present as an array (never null/omitted, even when empty) — safe to check
   * `.length` directly (B9-backend). */
  blockers?: RunBlocker[];
}

// --- Phase 5: providers + analysis (mirrors internal/httpapi/providers.go, analysis.go) ---

export interface Provider {
  id: number;
  name: string;
  kind: string; // "anthropic-compat" | "openai-compat"
  base_url: string;
  api_key_hint: string; // e.g. "…2345" — full keys never leave the server
  models: string[];
  requests_per_second: number;
  burst: number;
  enabled: boolean;
  last_verified_at?: string;
}

/** POST /api/providers/{id}/test — ok=false is still HTTP 200; show `error`. */
export interface ProviderTestResult {
  ok: boolean;
  models?: string[];
  tested_model?: string;
  error?: string;
}

/** Per problem × method-version score rollup. All *_total fields are decimal strings. */
export interface MethodProblemStat {
  problem_id: number;
  problem_number: number;
  max_points: string; // decimal string
  method_version_id: number;
  /** The method behind method_version_id (analysis redesign 2026-07-11, B1) —
   * lets consumers dedupe/rollup by method, e.g. the final-source picker. */
  method_id: number;
  method_name: string;
  method_version: number;
  policy: string; // "lenient" | "standard" | "strict"; empty for legacy (pre-D25) configs
  records: number;
  mean_total: string; // decimal string ("" when unknown)
  median_total: string; // decimal string
  stddev_total: string; // decimal string
  zeros: number;
  maxes: number;
  conf_high: number;
  conf_medium: number;
  conf_low: number;
  conf_illegible: number;
  input_tokens: number;
  output_tokens: number;
}

export interface HumanAgreementRow {
  problem_number: number;
  method_version_id: number;
  method_name: string;
  method_version: number;
  pairs: number;
  mean_abs_diff: string; // decimal string
  exact_matches: number;
  within_one: number;
}

/** Per-problem set of distinct policies among the methods that graded it (D25). */
export interface PolicyMixRow {
  problem_id: number;
  problem_number: number;
  policies: string[];
}

// AnalysisResponse itself is defined below (Task S4 block) — it gained override_rate.

/**
 * GET /api/assessments/{id}/totals row (internal/httpapi/review.go
 * handleAssessmentTotals, backed by the AssessmentStudentTotals query): one row per
 * rostered student with an answer, summed across all problems. `total` is absent
 * until at least one answer has an official record set (D3 — never a silent 0).
 */
export interface AssessmentTotalRow {
  student_id: string;
  name: string;
  answers: number;
  graded: number;
  total?: string; // decimal string
  /** Roster-lifecycle (plan 2026-07-10): withdrawn students stay in totals with an
   * explicit marker — never silently dropped (locked semantics e). */
  withdrawn?: boolean;
}

export interface AssessmentTotalsResponse {
  students: AssessmentTotalRow[];
}

// --- consensus (mirrors internal/httpapi/aggregation.go; semantics in DECISIONS.md D17) ---

/** The three answer flags aggregation owns — it adds and clears them on each re-run. */
export type AggFlag = "agg_disagreement" | "agg_missing" | "agg_low_confidence";

export interface AggregationPolicy {
  method_version_ids: number[];
  combiner: "majority" | "mean";
  fault_tolerance: number;
  flag_triggers: string[]; // subset of AggFlag
  set_official: boolean;
}

/** GET/PUT /api/assessments/{id}/aggregation */
export interface AggregationPolicyResponse {
  policy: AggregationPolicy | null;
}

/** POST /api/assessments/{id}/aggregate */
export interface AggregationReport {
  answers_considered: number;
  aggregates_written: number;
  officials_set: number;
  flagged: Partial<Record<AggFlag, number>>;
}

/** GET /api/problems/{id}/prompt-preview — the exact prompt a method would send. */
export interface PromptPreview {
  system: string;
  user: string;
  schema: string; // output JSON schema, serialized
  pins: {
    rubric_version: number;
    reference_solution_version: number; // 0 = not included
    prompt_template: string;
    prompt_template_version: number;
    provider: string;
    model: string;
    temperature: number;
    policy: string; // "lenient" | "standard" | "strict"
  };
}

// --- page-level scan intake (mirrors internal/httpapi/scans.go, internal/scan/finalize.go;
// design spec docs/superpowers/specs/2026-07-04-page-level-scan-intake-design.md). The
// upload/review/reconciliation types are gone with the file-level pipeline they described —
// recreated in Task 11 for the page-level flow. ------------------------------------------

/** Normalized 0..1 page coordinates; one region per kind, applied to EVERY page. */
export type IDRegionKind = "student_id" | "name" | "problem_id";

export interface IDRegion {
  kind: IDRegionKind;
  x: number;
  y: number;
  w: number;
  h: number;
  color: string;
  padding: number;
}

/** scanBatchJSON (handleCreateScanBatch/handleListScanBatches). */
export interface ScanBatch {
  id: number;
  assessment_id: number;
  ocr_enabled: boolean;
  ocr_provider?: string;
  ocr_model?: string;
}

/** scanBatchListRowJSON (handleListScanBatches): scanBatchJSON embedded + page-state
 * counters from one grouped progress query (no N+1, no OCR/PII columns). */
export interface ScanBatchListRow extends ScanBatch {
  total_pages: number;
  processing_pages: number;
  orphan_pages: number;
  assigned_pages: number;
  parked_pages: number;
  discarded_pages: number;
  errored_pages: number;
}

/** deriveScanPageState precedence: error > discarded > promoted > parked > assigned >
 * orphan (identified_at set) > processing. */
export type ScanPageState =
  | "processing"
  | "orphan"
  | "assigned"
  | "parked"
  | "promoted"
  | "discarded"
  | "error";

/** scanPageJSON (handleListScanPages). All optional fields are Go omitempty — absent, not
 * null, when unset. proposed_problem_id/assigned_problem_id are 0-omitted (a real id is
 * never 0 in this schema). */
export interface ScanPage {
  id: number;
  batch_id: number;
  page_index: number;
  state: ScanPageState;
  error?: string;
  ocr_student_id?: string;
  ocr_name?: string;
  ocr_problem?: string;
  ocr_engine?: string;
  proposal_source?: string;
  proposed_student_id?: string;
  proposed_name?: string;
  proposed_problem_id?: number;
  assigned_student_id?: string;
  assigned_name?: string;
  assigned_problem_id?: number;
  assigned_by_user: boolean;
  parked_reason?: "duplicate" | "conflict";
  parked_against?: number;
  discard_reason?: string;
  has_image: boolean;
}

/** scan.SkipInfo — one source file the batch-create pass declined to stage. */
export interface SkipInfo {
  filename: string;
  reason: string;
}

/** POST /api/assessments/{id}/scan-batches 200 response. 409 body (regions_incomplete)
 * is handled ad hoc at the call site (ApiError.details), not modeled as a type. */
export interface CreateScanBatchResponse {
  batch: ScanBatch;
  created: number;
  skipped: SkipInfo[];
}

export type MatrixCellState = "empty" | "auto" | "manual" | "promoted" | "submitted";

/** matrixCellJSON — page_id is Go omitempty (0 omitted; a real page id is never 0). */
export interface MatrixCell {
  problem_id: number;
  state: MatrixCellState;
  page_id?: number;
}

export interface MatrixRow {
  student_id: string;
  name: string;
  cells: MatrixCell[];
}

/** GET /api/assessments/{id}/scan-matrix response (matrixJSON). */
export interface ScanMatrix {
  problems: { id: number; number: number }[];
  rows: MatrixRow[];
}

/** POST /api/assessments/{id}/scan-finalize 202 response (scan.FinalizeReport). A 409
 * (missing cells unacknowledged) carries {error, missing_cells} instead of this shape. */
export interface FinalizeReport {
  enqueued: number;
  already_promoted: number;
  missing_cells: number;
}

/** GET /api/assessments/{id}/scan-missing row (handleScanMissing; ad hoc map, not a
 * named Go struct, but the field set is fixed). */
export interface MissingCell {
  student_id: string;
  name: string;
  problem_number: number;
}

// =====================================================================================
// --- Task S3: cost caps/estimates, spot-check gate, score distributions --------------
// (mirrors internal/httpapi/runs.go, providers.go pricing, analysis.go
// handleScoreDistribution — trust spec §2-5). NOTE: a concurrent task may also append
// types below this block; keep S3's additions contained here.
// =====================================================================================

/** GET/PUT /api/providers/{id}/pricing row (model_pricing table, trust spec §2). */
export interface ModelPricing {
  model: string;
  input_usd_per_mtok: string; // decimal string, $/Mtok
  output_usd_per_mtok: string; // decimal string, $/Mtok
}

/**
 * POST /api/runs 409 body when launching would exceed ADAMARKER_MONTHLY_BUDGET_USD
 * (trust spec §3, D36). All fields are decimal-string USD amounts.
 */
export interface BudgetExceededError {
  error: string;
  month_to_date: string;
  estimate: string;
  budget: string;
}

/**
 * POST /api/runs/{id}/accept-official 409 body when the spot-check gate isn't open yet
 * (trust spec §4, D37): `done` of `total` sampled records still need a verdict.
 */
export interface SpotCheckPendingError {
  error: string;
  total: number;
  done: number;
}

/** One sampled grading record awaiting (or holding) a spot-check verdict. */
export interface SpotCheckSample {
  id: number;
  grading_record_id: number;
  answer_id: number;
  problem_number: number;
  total?: string; // decimal string
  confidence?: string;
  verdict?: "agree" | "adjusted";
  note: string;
  checker_id?: number;
}

/** GET /api/runs/{id}/spot-check response. */
export interface SpotCheckResponse {
  samples: SpotCheckSample[];
  state: { total: number; done: number; waived: boolean };
  agreement: { agreed: number; total: number };
}

/** Score-distribution source: preferred official grades, or a labeled AI-only fallback. */
export type DistributionSource = "official" | "ai_fallback" | "none";

export interface DistributionTotals {
  n: number;
  mean: string; // decimal string
  stddev: string; // decimal string
  zeros: number;
  maxes: number;
  zero_pct: string; // decimal string, e.g. "12.5"
  max_pct: string; // decimal string
}

export interface DistributionCriterion {
  criterion_id: number;
  description: string;
  points: string; // decimal string
  n: number;
  mean: string;
  stddev: string;
  zeros: number;
  maxes: number;
  zero_pct: string;
  max_pct: string;
}

/** GET /api/problems/{id}/score-distribution response. */
export interface ScoreDistributionResponse {
  problem_id: number;
  max_points: string; // decimal string
  source: DistributionSource;
  total: DistributionTotals | null;
  histogram: number[]; // 10 buckets, evenly spanning [0, max_points]
  criteria: DistributionCriterion[];
}

// =====================================================================================
// --- Task S4: override rate + cost reports, audit log viewer ------------------------
// (mirrors internal/httpapi/analysis.go handleAssessmentAnalysis "override_rate" block
// and internal/httpapi/audit.go handleListAudit — trust spec §6/§7, landed a3a6b96).
// =====================================================================================

/**
 * Per-method override rate row (analysis.go overrideRate, trust spec §7, D40). Only
 * emitted for a method_version that has at least one answer with BOTH a model record
 * from this method AND an official record set — `answers` is therefore always >=1.
 * A method with zero eligible answers (no official set yet for anything it graded)
 * has NO row here at all: absence means "no officials yet to compare against", never
 * "never overridden" — the UI must not default a missing method to 0%.
 */
export interface OverrideRateRow {
  method_version_id: number;
  method_name: string;
  method_version: number;
  answers: number;
  human_overrides: number;
  scored_disagreements: number; // human replaced a real AI score
  filled_blanks: number; // human filled a cell the AI abstained on (illegible)
  override_rate: string; // decimal string in [0,1]
  mean_abs_diff: string; // decimal string
}

// --- method disagreement (analysis redesign 2026-07-11, B1: analysis.go
// "disagreement" block). Same record-selection semantics as `agreement` (latest
// model record per answer × method-version, current rubric only), so the numbers
// never contradict the tables next to them. Both arrays are always present and
// BOTH empty when fewer than two method-versions have comparable records — the
// frontend's signal to hide the section entirely. ------------------------------------

/** One problem's between-method spread rollup. Scores/spreads are decimal strings. */
export interface DisagreementProblemRow {
  problem_id: number;
  problem_number: number;
  max_points: string; // decimal string
  answers_compared: number;
  median_spread: string; // decimal string
  /** Answers whose spread ≥ GREATEST(1, 0.1 × max_points) — the "big gap" threshold
   * the matrix's "methods split" flag shares. */
  big_gap_count: number;
}

/** One method-version's total on a top-gap answer. */
export interface DisagreementScore {
  method_version_id: number;
  method_name: string;
  total: string; // decimal string
}

/** One of the ≤10 widest per-answer gaps. student_display is the roster's EXTERNAL
 * id — never the name (PII discipline in cross-tab payloads). */
export interface DisagreementAnswerRow {
  answer_id: number;
  student_display: string;
  problem_number: number;
  scores: DisagreementScore[];
  spread: string; // decimal string
}

export interface AnalysisDisagreement {
  problems: DisagreementProblemRow[];
  top_answers: DisagreementAnswerRow[];
}

/** AnalysisResponse gains override_rate alongside the S1-era stats/agreement/policy_mix,
 * and the disagreement block (analysis redesign 2026-07-11, B1). */
export interface AnalysisResponse {
  stats: MethodProblemStat[];
  agreement: HumanAgreementRow[];
  policy_mix: PolicyMixRow[];
  override_rate: OverrideRateRow[];
  disagreement: AnalysisDisagreement;
}

/** One audit_log row (auditJSON, trust spec §6, D39). detail is raw JSONB — render only,
 * never log (CLAUDE.md: audit rows carry actor emails). */
export interface AuditEntry {
  id: number;
  actor_user_id?: number;
  actor_email?: string;
  action: string;
  target_kind: string;
  target_id: string;
  detail?: unknown;
  created_at?: string;
}

/** GET /api/audit response (admin-only, newest-first, 50/page default). */
export interface AuditListResponse {
  entries: AuditEntry[];
}

// =====================================================================================
// --- Task S2: publish preview/publish/unpublish/batch history -----------------------
// (mirrors internal/httpapi/publish.go + internal/publish/publish.go Preview/
// PublishResult — trust spec §2/§3/§7). Points/money and student refs carry PII
// (names/emails) — never console.log these.
// =====================================================================================

/** publish.StudentRef — a roster student reference in preview/skip/changed lists. */
export interface PublishStudentRef {
  student_id: number; // internal id
  external_id: string;
  name: string;
}

/** db.PublishBlockersRow — one coverage-gate blocker (spec §2). `kind` includes the
 * fail-closed "not_ingested" case: an active roster student with zero answers rows. */
export interface PublishBlockerRow {
  answer_id: number;
  student_external_id: string;
  student_name: string;
  problem_number: number;
  problem_title: string;
  kind: string; // e.g. "ungraded" | "not_ingested"
}

/** GET /api/assessments/{id}/publish/preview response (publish.Preview). */
export interface PublishPreview {
  assessment_id: number;
  total_answers: number;
  graded: number;
  no_submission: number;
  blocked: number;
  not_ingested: number;
  publishable: boolean;
  blockers: PublishBlockerRow[];
  /** A live (non-superseded) batch exists — gates the "Already published" badge and the
   * Unpublish button (M1). */
  has_live_batch: boolean;
  /** Any batch ever existed (superseded or not) — drives changed-only re-publish. */
  ever_published: boolean;
  /** @deprecated mirrors has_live_batch; kept for back-compat. */
  already_published: boolean;
  changed?: PublishStudentRef[]; // vs latest batch, re-publish only
  skipped?: PublishStudentRef[]; // all-no_submission students
  student_count: number;
  email_disabled: boolean; // provider == none
  /** ADAMARKER_EMAIL_FROM (spec §2 D41); empty when unset (e.g. provider == none). */
  from: string;
  /** mirrors config.ReportFontConfigured() (spec §3 D43) — the attachment radio is
   * disabled with a hint when this is false, rather than letting the operator pick an
   * option the publish endpoint would then 400 on. */
  report_attachments_available: boolean;
  /** Problems with no assigned TA (spec §6 D60 publish-preview warning): if a student
   * escalates one of these to the final regrade turn, the handoff email has no
   * recipient. Warn-only — never blocks publish. Always present (possibly empty). */
  unassigned_ta_problems: UnassignedTAProblem[];
  /** The exam's chosen grading source (0027); null blocks publishing. */
  final_source: FinalSourceState | null;
  /** Publish-scoped workflow-guard warnings (plan 2026-07-10, Task B3). */
  warnings?: WorkflowWarning[];
}

/** publish.FinalSourceState — the chosen source + (method sources) the
 * relocated spot-check gate on that exact pinned completed run. */
export interface FinalSourceState {
  kind: "method" | "consensus";
  method_id?: number;
  method_name?: string;
  run_id?: number;
  run_status?: RunStatus;
  method_version?: number;
  scope_kind?: ScopeKind;
  scope_id?: number;
  spot_check_run_id?: number;
  spot_check_total?: number;
  spot_check_done?: number;
  spot_check_waived?: boolean;
  spot_check_open: boolean;
}

/** One entry of PublishPreview.unassigned_ta_problems (publish.UnassignedTAProblem). */
export interface UnassignedTAProblem {
  problem_id: number;
  problem_number: number;
}

/** Attachment mode sent on POST .../publish (spec §3 D44); "none" is the default. */
export type PublishAttachment = "none" | "compressed" | "original";

/** POST /api/assessments/{id}/publish 201 response (publish.PublishResult). */
export interface PublishResult {
  batch_id: number;
  items_created: number;
  enqueued: number;
  skipped: number;
  /** Previously-published students excluded from THIS batch because withdrawn
   * (roster-lifecycle plan 2026-07-10); always present, 0 on a first publish. */
  skipped_withdrawn: number;
  email_disabled: boolean;
  warning?: string;
}

/** POST /api/assessments/{id}/publish 409 body when a live batch already exists
 * (publish.ErrAlreadyPublished) — re-publish flow is unpublish then publish. */
export interface PublishAlreadyPublishedError {
  error: string;
}

/** POST /api/assessments/{id}/publish 409 body when the coverage gate isn't satisfied
 * (publish.ErrCoverageGate) — blockers echoed so the UI need not re-fetch preview. */
export interface PublishCoverageGateError {
  error: string;
  blockers: PublishBlockerRow[];
}

/** POST /api/assessments/{id}/publish 409 body when a changed-only re-publish finds
 * nothing changed (publish.ErrNothingToPublish). */
export interface PublishNothingToPublishError {
  error: string;
}

/** POST /api/assessments/{id}/unpublish response (admin-only). */
export interface UnpublishResult {
  unpublished_batch_id: number;
}

/** POST /api/assessments/{id}/materialize-answers response (roster-lifecycle plan
 * 2026-07-10, lecturer+): creates the missing answer rows for late-added active
 * students (the not_ingested dead end). Idempotent — a second call returns 0. */
export interface MaterializeAnswersResult {
  created: number;
}

/** Delivery lifecycle for one publish item. `claimed` and `sending` are active
 * worker-owned states. `uncertain` is terminal but dangerous: the provider may
 * already have accepted the message, so a resend can produce a duplicate. */
export type PublishEmailStatus =
  | "pending"
  | "claimed"
  | "sending"
  | "sent"
  | "failed"
  | "uncertain"
  | "skipped";

/** batchJSON.items row (handlePublishBatches itemJSON). */
export interface PublishBatchItem {
  id: number;
  student_id: string;
  student_name: string;
  recipient_email: string;
  email_status: PublishEmailStatus;
  provider_message_id?: string;
  error?: string;
  /** true when `error` carries the non-terminal report-attachment warning prefix
   * (publish.WarningPrefix) rather than a real send failure — email_status is still
   * "sent" in that case. Render amber, not red; strip the prefix for display. */
  warning: boolean;
}

/** batchJSON row (handlePublishBatches). */
export interface PublishBatch {
  id: number;
  note: string;
  resend_all: boolean;
  created_at?: string;
  superseded: boolean;
  attachment: PublishAttachment;
  zip: boolean;
  items: PublishBatchItem[];
  /** Per-status breakdown (B5, additive; always present, never omitted — safe to read
   * directly rather than re-deriving by filtering `items`). `items_count` may exceed
   * sent+failed+uncertain+skipped while deliveries are still pending/claimed/sending. */
  items_count: number;
  sent_count: number;
  failed_count: number;
  uncertain_count: number;
  skipped_count: number;
}

/** GET /api/assessments/{id}/publish/batches response. */
export interface PublishBatchesResponse {
  batches: PublishBatch[];
}

/** POST /api/publish/batches/{id}/resend-failed response. */
export interface ResendFailedResult {
  reenqueued: number;
  /** Failed items NOT re-enqueued because their student is withdrawn
   * (roster-lifecycle plan 2026-07-10); always present. */
  skipped_withdrawn: number;
}

/** POST /api/publish/items/{id}/resend response (spec §4 D46: terminal statuses only,
 * reuses the parent batch's attachment/zip settings). Resending an `uncertain` item
 * requires `{ acknowledge_duplicate_risk: true }`. */
export interface ResendItemResult {
  resent_item_id: number;
}

// =====================================================================================
// --- Regrade v2 (mirrors internal/httpapi/regrade.go regradeListJSON/regradeDetailJSON/
// regradeSubItemJSON, spec 2026-07-03-regrade-v2-design.md §5-§8). One request row per
// inbound email; kind (filed|addendum|unparsed|handed_off) + turn drive the queue;
// per-problem adjudication lives on `problems` sub-items. Subject/body/complaint_text
// are the student's actual email text — PII (CLAUDE.md): render as plain text only,
// NEVER dangerouslySetInnerHTML, NEVER console.log.
// =====================================================================================

/** Request lifecycle status (orthogonal to `kind`). The v1 escalated band and
 * rejected_rate_limited are gone (spec §4 D57 — the turn-token chain structurally
 * bounds volume); rejected_bad_token/superseded/sender_mismatch are unchanged ladder
 * rejections. */
export type RegradeStatus =
  | "received"
  | "under_review"
  | "resolved_upheld"
  | "resolved_regraded"
  | "rejected_bad_token"
  | "rejected_superseded"
  | "rejected_sender_mismatch";

/** Request kind (spec §2-§6, migration 0025): which bucket a reply landed in.
 * - filed: ≥1 valid `<pN>` block, token consumed, open for adjudication.
 * - addendum: a later reply to an already-consumed token — dimmed, no action.
 * - unparsed: 0 valid blocks, token NOT consumed — Send-reminder lives here.
 * - handed_off: consumed the final-turn (MAX+1) token — TAs notified, system silent. */
export type RegradeKind = "filed" | "addendum" | "unparsed" | "handed_off";

/** One adjudicated regrade overlay on an answer (rounds design, 0028). */
export interface RegradeLayer {
  turn: number;
  request_id: number;
  sub_item_id: number;
  verdict: "upheld" | "regraded";
  note: string;
  verdict_at?: string;
  adopted_record_id?: number;
  /** Present only for regraded verdicts — the overlay's total. */
  adopted_total?: string;
}

/** GET /api/assessments/{id}/regrade-rounds (regradeRoundJSON). */
export interface RegradeRound {
  turn: number;
  method_id?: number;
  method_name?: string;
  locked: boolean;
  pending: number;
  graded: number;
  adjudicated: number;
}

export interface RegradeRoundsResponse {
  rounds: RegradeRound[];
  regrade_max: number;
  regrade_deadline?: string;
  deadline_passed?: boolean;
}

/** GET /api/regrades row (regradeListJSON) — no body text, queue-table only. `turn` is
 * absent for rows whose token never parsed (e.g. rejected_bad_token). */
export interface RegradeListRow {
  id: number;
  status: RegradeStatus;
  kind: RegradeKind;
  turn?: number;
  assessment_id?: number;
  assessment_name?: string;
  student_external_id?: string;
  student_name?: string;
  /** Roster-lifecycle (plan 2026-07-10): the linked student is withdrawn (停修) —
   * regrade rights are preserved, the queue just flags the status. False/absent for
   * rows with no linked student. */
  student_withdrawn?: boolean;
  received_at?: string;
  subject: string;
}

/** GET /api/regrades response (I5: paginated). `total` counts every row the
 * current filters match across all pages — it drives the numbered pager. */
export interface RegradeListResponse {
  regrades: RegradeListRow[];
  limit?: number;
  offset?: number;
  total?: number;
  has_more?: boolean;
}

/** One per-criterion line of the embedded AI re-grade record (aiRecordCriterionJSON).
 * name/max are resolved against the record's own rubric version server-side; comment is
 * the model's rationale — student-content-adjacent, render only, never console.log. */
export interface RegradeAIRecordCriterion {
  criterion_id: number;
  name: string;
  score: string; // decimal string
  max: string; // decimal string
  comment?: string;
}

/** A sub-item's embedded ai_record (aiRecordJSON): the stricter AI re-grade outcome for
 * THAT problem's contested answer. answer_id is the AnswerView deep-link target.
 * criteria is null (not []) when the record's criterion_scores JSON failed to parse
 * server-side. */
export interface RegradeAIRecord {
  answer_id: number;
  criteria: RegradeAIRecordCriterion[] | null;
  total: string | null; // decimal string
  policy?: string; // "lenient" | "standard" | "strict"
  created_at?: string;
}

/** One contested problem within a request (regradeSubItemJSON, spec §5/§8).
 * verdict is null/absent until a TA adjudicates; ai_record/ai_error are per-sub-item
 * (the v1 "fan out to all answers" AI re-grade is gone — one job per sub-item, scoped to
 * THAT problem's complaint only). complaint_text is PII — render as plain text only. */
export interface RegradeSubItem {
  id: number;
  problem_id: number;
  problem_number?: number;
  complaint_text: string;
  verdict?: "upheld" | "regraded";
  verdict_note?: string;
  verdict_by?: number;
  verdict_at?: string;
  ai_error?: string;
  ai_record?: RegradeAIRecord;
}

/** GET /api/regrades/{id} (regradeDetailJSON). `snapshot` is a Go []byte field, which
 * encoding/json marshals as a base64 string — NOT raw embedded JSON — so the frontend
 * must base64-decode then JSON.parse it (see lib/api.ts decodeRegradeSnapshot) to get a
 * PublishSnapshot. Body/subject/from_email/problems[].complaint_text are the student's
 * own message text: render as plain text only, never dangerouslySetInnerHTML, never
 * logged. */
export interface RegradeDetail {
  id: number;
  status: RegradeStatus;
  kind: RegradeKind;
  turn?: number;
  regrade_max: number;
  from_email: string;
  spf_verdict?: string;
  dkim_verdict?: string;
  subject: string;
  body: string;
  received_at?: string;
  resolver_id?: number;
  resolution_note?: string;
  resolved_at?: string;
  /** Send-failure recovery marker (whole-branch review F1): set only once result email
   * #N was actually DELIVERED. A resolved request (resolved_upheld/resolved_regraded,
   * kind filed) with this still absent/null means the provider send failed after the
   * atomic resolve flip — the student never got the result — and
   * POST /api/regrades/{id}/resend-result can recover it. Always absent for a request
   * that was never resolved. */
  result_sent_at?: string | null;
  publish_item_id?: number;
  student_id?: number;
  student_external_id?: string;
  student_name?: string;
  assessment_id?: number;
  assessment_name?: string;
  snapshot?: string; // base64-encoded PublishSnapshot JSON — decode before use
  problems: RegradeSubItem[];
}

/** PATCH /api/regrades/{id}/problems/{pid} {outcome, note} request (spec §5/§8): one
 * sub-item's verdict. Response is the updated RegradeSubItem. */
export interface RegradeVerdictRequest {
  outcome: "upheld" | "regraded";
  note: string;
}

/** POST /api/regrades/{id}/problems {problem_id, complaint} request (spec §5 escape
 * hatch, TA+): manually add/correct a sub-item on a FILED request. Response is the
 * created RegradeSubItem. */
export interface RegradeAddProblemRequest {
  problem_id: number;
  complaint: string;
}

/** POST /api/regrades/{id}/send-result response (spec §5, TA+): 200 once every sub-item
 * is verdicted; 409 is the structured RegradeSendResult409Body below (regrade v2 gap 3). */
export interface RegradeSendResultResponse {
  id: number;
  turn: number;
  final: boolean;
  status: RegradeStatus;
}

/** One entry of a send-result 409's `unverdicted` list (unverdictedProblemJSON, regrade
 * v2 gap 3) — just enough for the per-problem checklist to key off. */
export interface UnverdictedProblem {
  problem_id: number;
  problem_number: number;
}

/** POST /api/regrades/{id}/send-result 409 body (apiError409Unverdicted, regrade v2 gap
 * 3): the AUTHORITATIVE list of sub-items still lacking a verdict, straight from the
 * server's own gate check — supersedes deriving this client-side from stale detail
 * state. */
export interface RegradeSendResult409Body {
  error: string;
  unverdicted: UnverdictedProblem[];
}

/** POST /api/regrades/{id}/remind response (spec §7, TA+, unparsed rows only, once). */
export interface RegradeRemindResponse {
  id: number;
  reminded: boolean;
}

/** POST /api/regrades/{id}/resend-result response (whole-branch review F1 recovery
 * path, TA+): re-sends result email #N for a resolved request whose result was never
 * actually delivered (result_sent_at was null). 409 when not eligible (still open, or
 * already delivered) — see RegradeDetail.result_sent_at doc. */
export interface RegradeResendResultResponse {
  id: number;
  resent: boolean;
}

/** PUT /api/problems/{id}/ta {user_id} response (spec §6/§8, lecturer+). user_id null
 * unassigns. */
export interface ProblemTAResponse {
  problem_id: number;
  user_id: number | null;
}

/** One row of GET /api/assessments/{id}/ta-assignments (taAssignmentJSON, regrade v2
 * gap 1, TA+). Every problem in the assessment is present — an unassigned problem still
 * has a row, with user_id/user_name both null (not omitted). user_name is the
 * assignee's display name — PII-adjacent (a colleague's name, not a student's), render
 * only, never console.log. */
export interface TAAssignmentRow {
  problem_id: number;
  problem_number: number;
  user_id: number | null;
  user_name: string | null;
}

/** GET /api/assessments/{id}/ta-assignments response (regrade v2 gap 1, TA+ — same
 * gating as the regrade queue's other read routes). */
export interface TAAssignmentsResponse {
  assignments: TAAssignmentRow[];
}

/** One row of GET /api/graders (graderJSON, regrade v2 gap 2, lecturer+): the
 * TA-assignment picker's data source, deliberately minimal — no email/other PII beyond
 * name, distinct from the full admin-only GET /api/users payload. */
export interface GraderRow {
  id: number;
  name: string;
  role: string;
}

/** GET /api/graders response (regrade v2 gap 2, lecturer+). */
export interface GradersResponse {
  graders: GraderRow[];
}

/** POST /api/regrades/{id}/ai-regrade?rerun=0|1 {problem_id} request + 202 response
 * (spec §8, re-scoped to one sub-item per job). */
export interface RegradeAIRequest {
  problem_id: number;
}

export interface AIRegradeEnqueueResult {
  id: number;
  sub_item_id: number;
  enqueued: number;
}

/** POST /api/regrades/ai-regrade-all 202 response (spec §8 D52). estimated_cost is a
 * decimal USD string, or "" when no model pricing resolved — render "unknown", never $0
 * (D35). The monthly-budget 409 body is the same BudgetExceededError run creation uses.
 * dry_run is present (true) only when the request itself was a dry-run preview
 * ({dry_run: true} in the request body, spec D52 §8's pre-confirm estimate) — the real
 * enqueue response omits the field entirely. */
export interface AIRegradeAllResult {
  enqueued: number;
  skipped: number;
  estimated_cost: string;
  dry_run?: boolean;
}

/** publish.SnapCriterion — one rubric criterion's scored line in a snapshot. */
export interface PublishSnapCriterion {
  name: string;
  score: string; // decimal string
  max: string; // decimal string
}

/** publish.SnapProblem — one problem's line within a published snapshot. */
export interface PublishSnapProblem {
  number: number;
  title: string;
  max: string; // decimal string
  no_submission: boolean;
  total: string; // decimal string; "" if ungraded/no_submission
  comment: string;
  criteria: PublishSnapCriterion[];
}

// =====================================================================================
// --- Workflow guards (plan 2026-07-10, Task F1): shared warning shape ----------------
// (mirrors internal/httpapi/warnings.go WorkflowWarning; emitted by
// GET /api/assessments/{id}/workflow-warnings and embedded in the run/publish
// previews). Frontend copy per code lives in src/lib/warnings.tsx warningView.
// =====================================================================================

export interface WorkflowWarning {
  code: string;
  severity: "info" | "warning" | "danger";
  count?: number;
  detail?: string;
}

/** publish.Snapshot — the frozen record the student was emailed at publish time
 * (decoded from RegradeDetail.snapshot). */
export interface PublishSnapshot {
  assessment_name: string;
  student_external_id: string;
  student_name: string;
  total: string; // decimal string; "" if none graded
  max: string; // decimal string
  all_no_submission: boolean;
  problems: PublishSnapProblem[];
}

// =====================================================================================
// --- LaTeX transcription export (spec docs/superpowers/specs/
// 2026-07-25-latex-transcription-export-design.md) — professor-facing, outside the
// grading workflow. One ZIP per (assessment, problem).
// =====================================================================================

/** One problem's row in GET /api/assessments/{id}/transcription-status.
 *
 * `cached + pending === answers`: transcriptions are cached content-addressed, so
 * `pending` is exactly the set that a download would have to pay a provider for.
 * `est_cost_usd` is a decimal USD string covering only those pending answers — when
 * `pending` is 0 the download is genuinely free (render "free", not "$0.0000"), and an
 * empty string means no pricing resolved (render "unknown", never a fake $0 — D35).
 *
 * `ready` is false while any of this problem's pages still lacks an accepted mask
 * (`pages_pending_mask` counts them): both ZIP endpoints refuse with 409 rather than
 * ship a bundle containing unmasked identities (spec §6.1), so the row offers no
 * download until the masks land. */
export interface TranscriptionStatusRow {
  number: number;
  title: string;
  answers: number;
  cached: number;
  pending: number;
  est_cost_usd: string;
  ready: boolean;
  pages_pending_mask: number;
}

/** Gate counts behind the export card's ladder (spec §6.1): each one is the count the
 * corresponding "waiting on …" line reports, so the UI never has to derive a stage from
 * a second endpoint. */
export interface TranscriptionGates {
  problems: number;
  students_total: number;
  students_with_work: number;
  pages_total: number;
  pages_mask_accepted: number;
}

/** GET /api/assessments/{id}/transcription-status.
 *
 * `ready` is the exam-level gate — true only when problems exist, some student work
 * exists, and every page's mask is accepted; the entire-exam ZIP is offered exactly
 * then. `verified` reports whether a compile gate (tectonic) is available on this
 * server; when false the `.tex` still exports but is marked unverified in the manifest.
 * `configured` reports whether a transcription model is configured at all. */
export interface TranscriptionStatusResponse {
  model: string;
  verified: boolean;
  configured: boolean;
  ready: boolean;
  gates: TranscriptionGates;
  /** Answers across the whole exam that a full-exam download would have to pay for. */
  total_pending: number;
  /** Decimal USD string for `total_pending`; "" when no pricing resolved (→ "unknown"). */
  total_est_cost_usd: string;
  problems: TranscriptionStatusRow[];
}

// =====================================================================================
// --- Per-student page (spec docs/superpowers/specs/2026-07-28-student-page-design.md)
// Read-only, staff-facing: GET /api/students/{sid} is the cheap summary (header +
// per-assessment scores), GET /api/students/{sid}/assessments/{aid} the lazy expanded
// detail (publish/delivery, provenance, regrades). Every score is a decimal string and
// is NULL — never a fake 0 (D3/D4) — when nothing official exists; the page renders "—"
// (ungraded) or "absent" (no answer row at all). Student names/emails and regrade
// verdicts are PII: render only, never console.log (CLAUDE.md).
// =====================================================================================

/** Header block of GET /api/students/{sid}. `student_id` is the school ID (the same
 * vocabulary as the totals table, the CSV, and this page's route), not the DB id. */
export interface StudentProfile {
  student_id: string;
  name: string;
  email: string;
  withdrawn: boolean;
}

/** One problem line of a StudentAssessmentRow. `answer_id` null means the student has no
 * answer row for that problem at all (absent — the row is not clickable); `score` null
 * means nothing official was recorded yet. */
export interface StudentAssessmentProblem {
  number: number;
  title: string;
  answer_id: number | null;
  score: string | null; // decimal string
  max: string; // decimal string
}

/** One assessment's collapsed-card summary (newest first in server order). `total` is
 * null until an official record exists for at least one problem. */
export interface StudentAssessmentRow {
  assessment_id: number;
  name: string;
  kind: string; // "exam" | "assignment"
  answers: number;
  graded: number;
  total: string | null; // decimal string
  max: string; // decimal string
  published: boolean;
  problems: StudentAssessmentProblem[] | null;
}

/** GET /api/students/{sid} — 404 when no student carries that school ID. */
export interface StudentPageResponse {
  student: StudentProfile;
  assessments: StudentAssessmentRow[] | null;
}

/** Publish + delivery state for one (student, assessment). Null on the detail response
 * when this student was never in a publish batch for the assessment — the page then
 * renders nothing at all (no badge, no delivery line), never a "not published" claim.
 * `changed_since_publish` is the "what the student believes vs. what is true" flag:
 * the official grade now differs from `snapshot_total`, the frozen total they were
 * emailed. */
export interface StudentPublishState {
  batch_created_at?: string;
  email_status: PublishEmailStatus;
  sent_at?: string | null;
  recipient_email: string;
  snapshot_total: string | null; // decimal string
  changed_since_publish: boolean;
}

/** Per-problem provenance from the expanded detail, keyed back to the summary rows by
 * `number`. `source` is the official record's origin ("human" | "model" | "aggregate");
 * `model_id` is set only for model records — a bare overridden score that reads as
 * hand-given is a lie by omission, so both are rendered. `published_score` is what the
 * publish snapshot said, and differs from `current_score` exactly when `changed`. */
export interface StudentProblemProvenance {
  number: number;
  answer_id: number | null;
  source: string | null;
  model_id: string | null;
  confidence: string | null;
  flags: string[] | null;
  published_score: string | null; // decimal string
  current_score: string | null; // decimal string
  changed: boolean;
}

/** One contested problem inside a StudentRegradeRow. `verdict` stays null until a TA
 * adjudicates it. */
export interface StudentRegradeProblem {
  number: number;
  verdict: string | null; // "upheld" | "regraded"
}

/** One regrade request touching this (student, assessment). `status` carries whatever
 * vocabulary the endpoint reports (the queue's RegradeStatus values or condensed forms
 * like "resolved") — unknown values render verbatim, per lib/labels.ts convention. */
export interface StudentRegradeRow {
  request_id: number;
  received_at?: string;
  status: string;
  problems: StudentRegradeProblem[] | null;
}

/** GET /api/students/{sid}/assessments/{aid} — everything that is not a score. */
export interface StudentAssessmentDetailResponse {
  publish: StudentPublishState | null;
  problems: StudentProblemProvenance[] | null;
  regrades: StudentRegradeRow[] | null;
}
