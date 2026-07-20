# Stability & efficiency audit — 2026-07-02

*Trigger: user-reported cursor jank + "check thoroughly". Method: live incident
forensics, instrumented UI session (layout-shift + DOM-mutation observers on a
running build), then a 4-dimension parallel audit (backend efficiency, DB schema,
stability/concurrency, frontend perf) in which **every finding was adversarially
verified against the code and live EXPLAIN before being accepted** — 26 raised,
24 confirmed, 2 refuted.*

## Live incident found & resolved during the audit

**Two stale `./bin/adamarker` zombie processes** (started 04:16 and 06:26, holding
no HTTP listener) ran full River worker fleets against the shared dev database for
~19 h. Confirmed damage: a `run.leaf` job failed with
`provider "openrouter": unknown kind "openai-compat"` (an error only pre-adapter
code can emit) and **2 model records written with `policy IS NULL`** after
migration 0012 (impossible under current code — i.e. stale code graded new jobs).
Their River pollers also kept the Docker VM at a constant ~40 % CPU, the most
likely cause of the system-wide cursor jank. Both processes were killed; the
systemic fix is F14 below (worker fencing, **the audit's only Critical**).

The instrumented UI session on the current build measured **zero** idle layout
shifts / DOM mutations / network churn on Methods, Runs, and the Identify tab —
the app at rest is clean; the remaining jitter mechanisms were the moving-target
findings F21–F23.

## Confirmed findings

Severity: C = critical, I = important, M = minor. Status: **fixed** (this
session), **chip** (spun off as a follow-up task), noted.

> **2026-07-03 close-out:** every chip has since landed — 21 of 24 findings fixed,
> 1 won't-fix-with-rationale (F7), 2 noted (F8-fold and F24 hygiene). The audit is done.

| # | Sev | Finding | Where | Status |
|---|-----|---------|-------|--------|
| F1 | I | Scan-batch **Finalize promotes all files synchronously in one HTTP request** — ~2000 PDFium doc-opens + ~1800 rasterizations ≈ tens of minutes vs the browser's ~300 s fetch abort; the flagship exam operation cannot complete at 200×9 scale without blind re-clicks | internal/scan/scan.go:1009, httpapi/scans.go:395 | **fixed** (D27: scan.promote jobs + 202/poll) |
| F2 | I | **ApplyMasks re-masks ~1800 pages synchronously in-request** (~180 ms/page measured) and resets review status as it goes; since the D10 gate blocks runs until masking completes, the whole grading workflow wedges at scale | internal/ingest/ingest.go:524 | **fixed** (D27: mask.page jobs + mask_input_sha skip) |
| F3 | I | **Every `RenderPage` re-copies the whole PDF into WASM and re-parses it** — an N-page ingest does N+1 full document loads (~40–100 GB of pure memcpy across a 200-file finalize) | internal/render/pdfium.go:75 | **fixed** (document-handle Renderer: Open/RenderPage×N/Close, pool-pinned) |
| F4 | I | **1 GiB zip buffered fully in heap, twice** (upload handler ReadAll + Expand ReadAll) — OOM-kill outage risk on a solo VM | httpapi/scans.go:232, scan.go:314 | **fixed** (streamed end-to-end; blobstore.RandomAccess + temp-file fallback) |
| F5 | I | **Global 30 s ReadTimeout kills large zip uploads mid-body** | cmd/adamarker/main.go:155 | **fixed** (per-request deadlines via ResponseController; upload routes extend to 20m) |
| F6 | M | `handleListScanBatches` N+1: fetches every scan_files row (incl. OCR PII columns) per batch to compute five counters — polled every 2 s during processing | httpapi/scans.go:294 | **fixed** (ScanBatchProgress grouped aggregate, no PII columns) |
| F7 | M | `MaterializeAnswers` runs the full roster×problems cross-join upsert **inside every per-file ingest tx** (200 promotions = 200 full materializations) | ingest.go:279 | won't-fix as-is: scoping it empirically breaks the D1 "no_submission answers visible from first ingest" contract (test-proven); the 200× repetition cost is a finalize-loop-structure issue and folds into F1's queue move |
| F8 | M | `RenderFile` re-decodes the full-page JPEG it encoded moments earlier just to crop the ID box | scan.go:494 | **fixed** (crop from the pre-encode raster via RenderPageImage + imaging.CropImage) |
| F9 | I | `grading_run_items` FKs (`answer_id`, `record_id`) unindexed — delete cascades go quadratic (live EXPLAIN: seq-scans) | migrations/0004:94 | **fixed** (0013) |
| F10 | I | `answers.official_record_id` FK unindexed — every grading_records delete seq-scans answers | migrations/0004:108 | **fixed** (0013) |
| F11 | M | `ExecuteAggregation` issues ~1800 sequential transactions and rewrites every answer row 3× per click (no-op flag removals still write) | grading/aggregate_run.go:125 | **fixed** (chunked txs of 50 + no-op flag guard) |
| F12 | M | `grading_records.rubric_version_id` FK unindexed | migrations/0004:64 | **fixed** (0013) |
| F13 | M | `scan_files_batch_idx` fully redundant with `UNIQUE(batch_id, source_sha256)` | migrations/0010:84 | **fixed** (0013) |
| F14 | **C** | **No single-worker-fleet guard**: any second binary against the DB silently grades with its own (possibly stale) code — proven live today | cmd/adamarker/main.go:129 | **fixed** (D26 advisory lock) |
| F15 | I | scan Expand/CreateBatch hold a pool connection + open tx **across blob writes** (up to 1 GiB, fsync per entry) — pool exhaustion risk | scan.go:325 | **fixed** (blobs written before the row tx; content-addressed page keys) |
| F16 | I | (composite of F1/F2/F5 — request-scoped heavy work) | httpapi/scans.go:395 | **fixed** (D27, with F1/F2) |
| F17 | I | **Shutdown insta-cancels in-flight 5-min jobs** (no River SoftStopTimeout; work ctx inherits the signal ctx) and a finalAttempt cancellation is recorded as a terminal grading failure — or wedges the run in `running` | grading/runner.go:212, queue/river.go | **fixed** (Start detached from signal; Shutdown drains ADAMARKER_SHUTDOWN_DRAIN then escalates; interruption → JobSnooze across all five job kinds, never terminal) |
| F18 | M | `localocr.Engine.Close` destroys the ORT session **without taking e.mu** — data race with `session.Run`, native segfault on the shutdown-timeout path | localocr/engine.go:174 | **fixed** |
| F19 | M | `registry.DBSource.Provider` holds the global cache mutex **across the DB fetch** — all workers' provider lookups serialize behind a stalled refresh | llm/registry/registry.go:71 | **fixed** |
| F20 | I | Run detail renders the full unbounded run-items table (~1800 rows) unmemoized on a 2 s poll | frontend Runs.tsx:345 | **fixed** (default failed/running items + all=1 escape hatch, memoized rows) |
| F21 | I | **ReviewStrip cursor is index-based over a polling, reordering list** — the file under review silently swaps; a TA can confirm the wrong proposal | ReviewStrip.tsx:134 | **fixed** |
| F22 | I | **SafeImage reserves no layout space** — every image load/swap/failure reflows the clickable content around it (a direct moving-click-target / cursor-jitter mechanism) | SafeImage.tsx:40 | **fixed** |
| F23 | M | `useSamplePage` swaps the Identify-tab sample image mid-processing on the 2 s poll (reflow) | useSamplePage.ts:28 | **fixed** |
| F24 | M | (dev-DB hygiene) test container at 731 MB / VM churn — resolved by zombie kill; recreate `db-test` when convenient | compose | noted |

**Refuted by verification** (kept for the record): `HumanAgreementForAssessment`
unbounded scan (real query-shape quirk, but the existing
`grading_records_answer_idx` keeps the plan cheap — verified by EXPLAIN ANALYZE
on a reproduced schema); `AnswerView` keydown resubscribe-per-render (true fact,
but keystrokes re-render only the child `GradeForm`, so the cost claim fails).

## Recommended follow-up order

1. **F1+F2 (+F16)** — move finalize-promotion and mask-application onto River
   (the queue exists; both loops are per-item idempotent already). This is the
   one that bites on exam day.
2. **F3+F8** — document-handle Renderer seam (open once, render N pages, crop
   before encode).
3. **F4+F5+F15** — stream zips end-to-end, per-route read deadlines, blob I/O
   out of transactions.
4. **F17** — shutdown drain (SoftStopTimeout ≥ job timeout; classify
   shutdown-cancellation as retryable, never a terminal grading failure).
5. **F6+F11+F20** — polling/aggregation polish.
