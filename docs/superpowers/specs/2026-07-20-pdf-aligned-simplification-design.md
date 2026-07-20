# PDF-aligned workflow simplification — design

*2026-07-20. Driver: the lecturer's accepted workflow is the TA guide
([`docs/ta_batch_grading_guide_v2.typ`](../../ta_batch_grading_guide_v2.typ), v2.1) — and the app
still makes TAs do things the guide had to apologize for. This round removes every apology:
the two hedged "規劃中功能" become real, the Overview workflow mirrors the guide's §9 stage
checklist exactly, and the UI stops contradicting the guide's product name. Deliberately
"slight": no schema rework beyond one CHECK extension, no new pages, no new concepts.*

## 1. Calibration sample runs (guide §3.1 hedge)

**Problem.** The guide's calibration stage needs 5–10 representative answers graded, but a run's
scope is `assessment | problem | answer` — so TAs hand-launch 5–10 single-answer runs. The guide
admits it: 「校準批目前需逐份啟動；「一鍵抽樣 N 份」為規劃中功能。」

**Design.** A fourth run scope, `sample`, where `scope_id` carries **N** (the sample size):

- Migration `0037_sample_scope.sql` widens the `grading_runs.scope_kind` CHECK to include
  `'sample'` (down-migration restores it; no rows to backfill).
- `Runner.Plan`'s scope switch gains `case "sample"`: resolve the assessment's gradeable answers
  (existing `AnswerIDsForAssessment` → `AnswersWithProblems` for problem ids), then take a
  **deterministic, problem-stratified** sample of `min(N, pool)` — round-robin across problems
  ordered by problem id, within each problem ordered by `SHA-256(runID ‖ answerID)` — the same
  determinism idiom as `SelectSpotCheckSample` (trust spec §4), seeded by `runID` so re-planning
  the same run yields the same set, and persisted as ordinary run items (so the sample is
  recorded, reproducible, and visible in the run detail).
- Launch gate + preview + launch warnings treat `sample` exactly like `assessment` (whole-
  assessment mask gate — calibration happens after mask review per the SOP; cost preview
  estimates `min(N, gradeable)` units). `N < 1` is a 400 at create time.
- Sample runs cannot be pinned as the final source — already enforced structurally by
  `ErrFinalRunNotAssessmentScope` (only assessment-scope runs pin). They are probes for the
  Analysis method cards, which need no change (they read records regardless of run scope).

## 2. Same-score side-by-side (guide §6.3 hedge)

**Problem.** The consistency check ("compare all 7-, 8-, 9-point answers") is a manual browse;
the guide admits 「同分並排檢視…一鍵並排為規劃中功能」.

**Design.** ProblemReview gains a URL-driven **By score** mode (`?view=scores`, keeping the
existing table as the default): the already-client-side student list groups by `official_total`
(descending; `ungraded/no_submission` bucketed last), each score bucket expandable to a
horizontal strip of answer cards — masked page thumbnail (`/api/answer-pages/{id}/image?variant=masked`),
student id, flags, link to the AnswerView. Strip capped at 12 cards with an explicit
"+K more — open the table filtered to this score" overflow link (no silent truncation). No new
backend endpoint: page ids come from a per-answer `GET /api/answers/{id}` fetch for the
expanded bucket only (bounded by the cap). Masked variant only — consistency checking does not
need identity, and the strip may be shown on a projector.

## 3. Overview = the guide's §9 checklist

The existing "Grading workflow" card (7 steps) diverges from the guide's stages in two places:

- **New step "Calibrate on a sample"** between *Mask identities* and *AI grading*: state derived
  from existing data (no calibration run yet → prompt; sample runs present → show latest run
  status + a link to Analysis for the AI-vs-human deltas). Primary action: an inline N input
  (default 8, the guide's 5–10 midpoint) + "Start calibration run" that launches a `sample`
  run via the same launch path (`/runs?launch=1&assessment_id=…&scope=sample&n=…` prefill).
- **New final step "Handle regrades"** after *Publish grades*: state = open regrade count for
  this assessment + whether a regrade deadline is set (the publish preview already computes
  the deadline warning); actions → Regrade rounds tab + Regrade inbox.

Step numbering shifts 7→9 steps; copy stays terse. No other Overview changes.

## 4. Product name (guide vs UI)

The guide (and README/go.mod) say **AdaGrade**; the SPA still displays "ADA-Marker" in the
sidebar, login, guide page, publish/identify copy, and helpContent (13 user-visible strings,
7 files). All display strings become "AdaGrade". Identifiers (`adamarker` binary, `ADAMARKER_*`
env, `ada-marker-frontend` package name, API paths) are untouched — operational names are out
of scope per the guide's 系統名稱 note.

## 5. Guide + in-app guide updates

- `ta_batch_grading_guide_v2.typ`: §3.1 hedge → describes the one-click sample-N launcher;
  §6.3 hedge → describes the By-score view; §9 開始前/校準批 rows updated accordingly; appendix
  changelog gains a line. Recompile the PDF.
- `GuidePage.tsx`: run-scope sentence gains `sample`; calibration paragraph drops "one at a
  time".

## Non-goals

Multi-sheet "append as extra page" (PLAN_GAPS W1 — its own design round), consensus/Analysis
changes, any nav restructuring beyond the two Overview steps, renaming operational identifiers.

## Testing

Sampler: pure-function unit tests (determinism, stratification, clamp, N≥pool). Plan: DB-backed
test creating a sample run over seeded answers → items match sampler output; mask-gate blocks
unreviewed. HTTP: create/preview validation (N<1 → 400; preview counts). Frontend: `tsc` +
build (repo has no FE test runner). Live: calibration run + By-score view exercised against the
seeded demo assessment.
