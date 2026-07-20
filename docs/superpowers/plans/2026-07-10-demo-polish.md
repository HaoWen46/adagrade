# Demo polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the five gaps noted while building the demo deck: no completed happy-path demo data, Providers table crowding, `method v-id` jargon in AnswerView, an under-emphasized cost estimate, and no nudge when publishing with no regrade deadline.

**Architecture:** Four small independent fixes plus one scripted task — `scripts/seed-demo-walkthrough.py` drives the public HTTP API against the dev server (:8899, dev login) to build "Demo Exam — completed": problems + rubrics + reference solutions, intake completed, masks applied and bulk-accepted, an AI grading run on the cheapest configured method, final source chosen, published (file email provider), and two regrade threads filed through the real inbound webhook. Everything through public APIs — the script is also an end-to-end smoke test.

## Global Constraints

- Go changes test-first; frontend gate `cd frontend && npx tsc --noEmit` (own files clean). No git commands — orchestrator commits. Edit only owned files.
- The dev server on :8899 (dev login, demo roster already imported) may be freely used and mutated by the SEED task. Never touch real student data; the demo roster is synthetic.
- Python: always `uv run` (house rule); prefer stdlib (urllib) so the script needs no deps.

## File Ownership

| Task | Files |
|---|---|
| SEED demo-walkthrough | **new** `scripts/seed-demo-walkthrough.py`, `scripts/dev-e2e.sh` (dev webhook secret), `Makefile` (a `demo-walkthrough` target), `docs/superpowers/specs/2026-07-04-guide-page-demo-data-design.md` (append a short section) |
| A providers-css | `frontend/src/pages/Providers.tsx` |
| B version-numbers | backend file exposing grading records (find it: grep `method_version_id` in internal/httpapi — likely review.go) + its test, `frontend/src/pages/AnswerView.tsx`, `frontend/src/lib/types.ts` (grading-record fields only) |
| C estimate-prominence | `frontend/src/pages/Runs.tsx` |
| D deadline-nudge | `internal/httpapi/publish.go` + test, `frontend/src/lib/warnings.tsx` |

### Task SEED: `scripts/seed-demo-walkthrough.py`

A deterministic, re-runnable driver (stdlib urllib + cookie jar; dev login as the bootstrap admin). If an assessment named **"Demo Exam — completed"** already exists, print its state and exit 0 (idempotence = skip, not merge). Steps, all via public API (read the handlers to get routes/payloads right; the existing artifacts in `data/demo/` are inputs):

1. Create the assessment + the 4 demo problems (statements from `scripts/make-demo-data.py`'s PROBLEMS) with 10 points each, a rubric per problem (2-3 criteria summing to 10, written to match the demo answers' quality tiers) and a short reference solution each.
2. Intake: use the **Submissions path** with per-student PDFs for determinism (generate them on the fly with reportlab via `uv run --with reportlab`, reusing make-demo-data.py's drawing helpers — import it as a module rather than duplicating; refactor it minimally if needed to expose functions, keeping its CLI behavior identical). Investigate how pages map to problems in this path (the ingest report shows MAPPED/EXPECTED) and drive whatever API completes the mapping so every answer has its page. If the Submissions path can't reach mapped-complete via API, fall back to the Identify path: upload the scan pile, then assign every page via `POST /api/scan-pages/{id}/assign` using generated ground truth (the script knows each page's student+problem — recompute the SEED=46 shuffle), then Finalize.
3. Masking: PUT the id-region-derived mask regions (or reuse the seeding the finalize path does), apply masks, poll until rendered, then `POST /api/assessments/{id}/masks/accept-pending`.
4. Materialize the non-demo roster students' answer rows (`POST /api/assessments/{id}/materialize-answers`) so coverage is clean (they become skipped).
5. AI grading: launch a run with the cheapest configured method (prefer the one whose model contains "flash"); poll until completed. `--no-ai` flag skips this and stops here with instructions. Spot-check gate: if publish requires spot-check verdicts on the latest run, submit accept verdicts via the API for the sampled records.
6. Final source: set it to the run's method. Publish: default attachment options, acknowledge via API exactly what the UI would send. The file email provider writes to the outbox — that's expected.
7. Regrades: ensure `ADAMARKER_INBOUND_WEBHOOK_SECRET` has a dev default in `scripts/dev-e2e.sh` (e.g. `dev-webhook-secret`, only when unset). If the running server lacks it (webhook 404s), print a clear message telling the operator to restart the server and re-run with `--regrades-only`. With the webhook live: extract two students' regrade tokens from their outbox .eml files (`data/outbox/`), POST two simulated Postmark inbound payloads — one contesting one problem, one contesting two problems with plausible complaint text — so the Regrade inbox and the assessment's rounds tab have real threads.
8. Print a summary table of what exists at the end. `make demo-walkthrough` wraps it.

Verification for this task: run the script for real against :8899 (it is expected to mutate the dev DB — that is the deliverable); assert via API at the end: coverage 100%, published batch live, 2 open regrade requests. Report the exact cost of the AI run from the run row.

### Task A: providers table crowding

At 1440px the MODELS column's chips collide with RATE. Give the models cell a max width + wrap (or truncate with title), and let the table breathe (`table-layout` or width hints). Verify with tsc; orchestrator checks visually.

### Task B: real version numbers in AnswerView

The grading-record JSON exposes `method_version_id`/`prompt_template_version_id` (DB ids). Add `method_version` and `prompt_version` (the human version integers) to the record payload by joining the version tables (TDD the handler change); type them in types.ts; AnswerView renders `method v{n} · prompt v{n}` instead of `method v-id {id} prompt v-id {id}`.

### Task C: cost estimate prominence

In the Launch dialog, promote the estimate line: "Estimated cost" as a small label with the dollar figure larger and semibold (text-base/semibold vs the current small muted line); when pricing is missing, render the "unknown (no pricing entered for this model)" state in amber with a link to `/providers`. Keep the layout compact — no new sections.

### Task D: regrade-deadline nudge

Publish preview gains warning code `no_regrade_deadline` (severity info): emitted when the email config has a reply channel (same condition that sets HasReplyTo on grade emails) AND `assessments.regrade_deadline` is NULL. TDD. Copy in warnings.tsx: "No regrade deadline is set — replies will be accepted indefinitely. Set one on the Regrade rounds tab." link `?tab=regrades`. (PublishTab already renders preview warnings — no other frontend change.)

## Verification gate (orchestrator)

`make vet && make test`; DB-backed suite; typecheck + build; run the seeder live; browser-check Providers/launch dialog/publish warning; regenerate the demo deck against the new state (add a Regrade-inbox slide); commit.
