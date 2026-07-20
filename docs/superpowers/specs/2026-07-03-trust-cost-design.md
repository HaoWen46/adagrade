# Trust & cost instrumentation — design

*2026-07-03 overnight session. Closes PLAN_GAPS B-H5 (cost caps), B-C5 (acceptance
gate), B-H14 (distribution sanity), the audit-log read path, and the Phase 8 subset
(override rate, cost per run). Same convention as the sibling spec: v0 defaults are
flagged (D35…) and harvested into DECISIONS.md for after-the-fact review.*

## 1. Goal

Before Phase 6 emails real students, AI grades need to be (a) financially bounded and
(b) evidentially trusted. Today `grading_records.cost_usd` is always NULL, nothing
stops a runaway spend, models that agree on the same misread auto-accept wrong grades,
and a whole-cohort systematic misread (`Problem 2 is all zeros`) is invisible until a
student replies.

## 2. Model pricing (D35)

New table `model_pricing`: `(provider_id FK, model TEXT, input_usd_per_mtok NUMERIC,
output_usd_per_mtok NUMERIC, updated_at)`, unique on (provider_id, model). Edited on
the existing Providers page (prices drift; MODELS.md already tells operators to check
current prices — now there's a place to put them). No seeding from MODELS.md: prices
are operator-entered data, not code.

`cost_usd` is computed at record-insert time from the record's token counts × the
pricing row (NULL when no pricing row exists — absence is visible, not zero). No
historical backfill in v0; a pricing edit affects only future records (flagged).

## 3. Budget caps (D36)

Two independent brakes, both fail-closed only when configured — an unconfigured system
behaves exactly as today:

- **Per-run cap:** nullable `runs.cost_cap_usd`, set at run creation in the UI. The
  leaf executor checks the run's accumulated `SUM(cost_usd)` before each grade call;
  at/over cap ⇒ remaining leaves record a terminal `budget_exceeded` failure reason and
  the run completes partially (retry-failed-only works after raising the cap).
- **Monthly global cap:** `ADAMARKER_MONTHLY_BUDGET_USD`. Run creation compares
  month-to-date `SUM(cost_usd)` + the new run's **pre-flight estimate** against it and
  refuses (409, with the numbers) when exceeded. Admins see the same 409 — raising the
  env var is the deliberate escape hatch.

**Pre-flight estimate:** per MODELS.md's own heuristic, `answers × (1500 input + 400
output tokens)` × pricing, summed per model in the method; shown on the run-creation
dialog whenever pricing exists, alongside month-to-date spend. An estimate with missing
pricing rows says so instead of printing a fake $0.

## 4. Spot-check gate (D37)

**Rule:** a run's grades cannot be bulk-accepted as official — via the Runs
"accept-official" action *or* an aggregation policy's auto-set-official path — until a
human has spot-checked a sample of that run's records. Per-answer manual acceptance in
AnswerView stays ungated (it *is* human review).

- **Sample:** deterministic PRNG seeded by run id (reproducible), stratified across the
  run's problems: `min(max(5, 5% of graded leaves), 20)` records.
- **Flow:** new `spot_checks` table (run_id, grading_record_id, verdict
  `agree|adjusted`, note, checker, created_at). Runs detail gains a "Spot-check"
  strip: image + AI grade side-by-side, agree/adjust per sample (adjust deep-links to
  the AnswerView manual-grade form). When all samples have verdicts, the gate opens;
  the accept-official confirm dialog shows the sample's agreement rate.
- **Override:** admin-only `POST /api/runs/{id}/spot-check/waive` with reason,
  audit-logged (`run.spotcheck.waive`).
- Existing runs created before this feature have no samples ⇒ gate would retroactively
  lock them; migration marks pre-existing completed runs as waived (`migration` reason)
  so history stays usable (flagged).

## 5. Distribution sanity view (B-H14) (D38)

New endpoint `GET /api/problems/{id}/score-distribution`: per-criterion and total —
mean, stddev, %zero, %max, and a 10-bucket histogram over official grades (fallback:
latest-run AI grades when officials are sparse, labeled as such). Surfaced twice:
- **ReviewTab:** compact histograms per problem replacing the bare counts.
- **Publish preview** (sibling spec): the same component, so "Problem 2 is all zeros"
  is staring at the operator right before they hit send.

## 6. Audit log read path (D39)

The write side exists (41+ action types). Add `GET /api/audit?target_kind=&target_id=
&action=&actor=&limit=&offset=` (admin-only; actions/targets only — `detail` JSONB is
included but the UI renders it collapsed). UI: an "Audit" section on the Users page
(it's already the admin corner), newest-first, filterable, 50/page. Plus a per-target
"history" drawer hook the AnswerView can adopt later.

## 7. Reports (Phase 8 subset) (D40)

Extend `analysis.sql` + AnalysisTab with:
- **Override rate per method:** share of answers where the official grade's source is a
  human record that *replaced or adjusted* an AI record from that method's runs, and
  mean |Δ| between AI total and final official total.
- **Cost per run:** run list gains `SUM(cost_usd)` + tokens; AnalysisTab shows cost per
  method per assessment and cost-per-answer.
Cross-exam comparisons stay deferred (Phase 8 remainder, in PLAN_GAPS).

## 8. Schema (migrations 0018, 0019)

0018_pricing_costcaps.sql: `model_pricing`, `runs.cost_cap_usd`.
0019_spot_checks.sql: `spot_checks` + the pre-existing-runs waiver backfill.

## 9. Testing

- Cost computation: pricing present/absent/edited-mid-run, NUMERIC rounding.
- Caps: leaf executor stops at cap (budget_exceeded is terminal + retryable),
  run-creation 409 math, estimate rendering with partial pricing.
- Spot-check: sample determinism + stratification, gate on both accept paths, waive
  audit, migration waiver.
- Distribution: known fixture distributions ⇒ exact stats; sparse-officials fallback
  labeling.
- Audit endpoint: RBAC (403 for non-admin), filters, pagination.
