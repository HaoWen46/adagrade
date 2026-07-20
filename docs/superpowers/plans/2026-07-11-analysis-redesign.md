# Analysis redesign + final-source picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the approved judge verdict (see `scratchpad` synthesis, reproduced in the task specs below): (1) the Final grading source picker disables record-less methods with a reason and annotates usable ones with counts, backed by a `final_source_no_records` warning code; (2) the Analysis tab is rebuilt around method report cards, a real per-answer disagreement section, one expandable problem matrix, and a collapsed run history.

**Architecture:** One backend foundation task owns all SQL: `method_id` added to the analysis stats, a `disagreement` block appended to the existing `GET /api/assessments/{id}/analysis` response, and the new warning code. Two frontend tasks then run in parallel: the AnalysisTab rebuild and the FinalSourceCard fix — both consuming the same enriched analysis payload.

## Global Constraints

- Go test-first. Integration tests: `docker compose up -d --wait db-test` + `ADAMARKER_TEST_DATABASE_URL=postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable go test ./internal/... -count=1 -run <Name>`.
- No git commands; orchestrator commits. Edit only owned files.
- **Decimal/points discipline (D4):** scores travel as decimal strings; client math via the existing bigint-micros helpers in `frontend/src/lib/decimal.ts` — never parseFloat for aggregation. Money stays strings.
- Columns and stats key on method-**versions** (consistent with `/analysis` and ConsensusTab); the existing `mixed_method_versions` warning is the safety net.
- Professor-friendly copy; no LLM jargon. `ui.tsx` primitives + existing `ScoreDistribution` only.
- `handleSetFinalSource` stays permissive — no new 4xx.

## File Ownership

| Task | Files |
|---|---|
| B1 foundation | `internal/store/queries/analysis.sql`, `internal/store/queries/warnings.sql`, regenerated `internal/store/db/*` (single `make sqlc`), the analysis store func + httpapi handler files (find via `grep -rn "analysis" internal/httpapi`), `internal/httpapi/warnings.go`, matching `*_test.go` |
| F1 analysis-tab | `frontend/src/pages/AnalysisTab.tsx` (+ optional new sibling components `frontend/src/pages/analysis/*.tsx`), `frontend/src/lib/types.ts`, `frontend/src/lib/warnings.tsx` |
| F2 picker | `frontend/src/pages/PublishTab.tsx` only |

### Task B1: analysis payload + warning code

- [ ] TDD: `method_id` added to the analysis stats SQL (`mv.method_id`, one line) and surfaced through the store row + JSON (`method_id`).
- [ ] TDD: append a `disagreement` object to the analysis response — no new route. Semantics MUST mirror the agreement CTE in `internal/store/queries/analysis.sql:32-54`: latest `source='model'` record per (answer, method-version), current rubric version only. Shape:
```json
"disagreement": {
  "problems": [{"problem_id":1,"problem_number":1,"max_points":"10","answers_compared":10,"median_spread":"1.5","big_gap_count":3}],
  "top_answers": [{"answer_id":7,"student_display":"B11902003","problem_number":3,"scores":[{"method_version_id":2,"method_name":"...","total":"4"}],"spread":"6"}]
}
```
big-gap threshold = `GREATEST(1, 0.1*max_points)`; `top_answers` capped at 10 ordered by spread desc; both arrays empty when <2 method-versions have records (frontend hide signal). All numerics that are scores/spreads = decimal strings. `student_display` = the student external id (no name — PII discipline in API payloads consumed cross-tab).
- [ ] TDD: `final_source_no_records` warning code (severity danger) in workflow-warnings: fires when `final_source_kind='method'` AND that method has zero `source='model'` records on this assessment. SQL in warnings.sql; clean-assessment case stays empty.

### Task F1: AnalysisTab rebuild

Rebuild per the approved layout (single file OK; split into `analysis/` siblings if >~800 lines). Sections top to bottom:

0. Existing PolicyMixWarning unchanged, first.
1. **Method report cards** — one Card per method-version with records (grid, `md:grid-cols-2 xl:grid-cols-4`; single method = one wide card). Rows: graded "40 of 40" (denominator from problems/summary); vs. hand grades = pairs-weighted mean |Δ| via micros helpers, subtext "exact N% · within 1 pt N% · N hand-graded answers", empty-state CTA "No hand grades yet — grade a few for calibration →" (existing onGoToReview); overridden (existing rateToPercent, absent = "no official grades yet"); confidence as ONE stacked 4-segment bar (high/medium/low/illegible, plain-word tooltips); cost total + per answer (no tokens). Green Badge "Closest to hand grades" on the best card ONLY when ≥2 methods each have ≥10 pairs, HelpTip explains criterion and n. Order: agreement asc when hand grades exist, else name.
2. **Where methods disagree** — rendered only when the `disagreement` arrays are non-empty. Headline count sentence, per-problem mini-table (problem, answers compared, median spread, big gaps), top-10 gap answers table with per-method totals and `/answers/{id}` links.
3. **Problem matrix** — ONE table: rows = problems, columns = one per method-version + Flags. Cell: mean over the existing MeanCell-style bar; line 2 xs "±0.8 vs hand (12)" or "—"; amber dot when (low+illegible)/records > 15%. Flags (client-computed, conservative): "many zeros" ≥30%, "everyone aced it" maxes ≥90%, "AI unsure" low+illegible ≥20%, "methods split" (per-problem big_gap_count/answers_compared > 20%), "mixed policies", "graded 38 of 40". Chevron row-expansion (collapsed default) contains: today's per-problem StatsTable verbatim, that problem's agreement rows, `ScoreDistribution` lazily (`enabled: expanded`) INCLUDING its currently-dropped `criteria` array, and that problem's largest-gap answers with links.
4. **Run history** — existing CostTable in a collapsed disclosure, fed by runs filtered to this assessment; delete the stale "most recent 50 system-wide" comment/caveats (check whether GET /api/runs supports assessment_id filtering; if not, keep client filtering but fix the copy).

Empty states: no records → today's empty card; records but no hand grades → cards with CTA, no badge, matrix line-2 "—", disagreement still renders.
`warnings.tsx`: copy for `final_source_no_records` → "The chosen final grading source hasn't graded this exam — nothing will become official. Pick a method that has grades on the Publish tab." link `?tab=publish`.
types.ts: analysis response additions (`method_id`, `disagreement`) exactly matching B1's JSON tags.

### Task F2: FinalSourceCard fix

In PublishTab.tsx only (analysis types come from F1's types.ts; if not landed yet, code to the plan shape — the gate reconciles):
- [ ] Method options: join `/api/methods` with the analysis stats rollup (dedupe by method via new `method_id`). With records → label `method — {name} · N answers graded`. Zero records → `<option disabled>` label `method — {name} — hasn't graded this exam`.
- [ ] Consensus option: disabled with label `consensus — set up the panel first` when `GET /api/assessments/{id}/aggregation` returns `policy: null`; otherwise enabled (n=1 panels are legal per D17), annotated `— not run yet` when no aggregate records are inferable from existing data (skip the annotation if not cheaply inferable).
- [ ] If the CURRENT source is a record-less method: keep it displayed/selected (never auto-change), render an amber note under the select: "This method hasn't graded this exam, so nothing will become official." (The standing warning code from B1 covers other surfaces.)

## Verification gate (orchestrator)

`make vet && make test`; DB-backed suite; typecheck + build; browser-verify: assessment 4 (1 method — cards degrade, no disagreement section, picker shows counts + disabled record-less method + disabled consensus), assessment 1 (2 methods with records — cards, disagreement, matrix columns), expanded matrix row, `final_source_no_records` fires when pointing assessment 4's source at the record-less method THEN RESTORE the correct source. Commit.
