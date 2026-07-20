# Overnight Build: Publish/Email/Regrade + Trust/Cost + Ops Kit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Phases 6–7 (publish + outbound grade email + inbound regrade queue), the trust & cost pass (pricing/caps/spot-check/distribution/audit-read), and the deployment ops kit (CI, deploy assets, backup/restore, ops status).

**Architecture:** Per the two committed specs —
[`2026-07-03-publish-email-regrade-design.md`](../specs/2026-07-03-publish-email-regrade-design.md)
and [`2026-07-03-trust-cost-design.md`](../specs/2026-07-03-trust-cost-design.md) —
which are **normative**: task briefs below give files/interfaces/tests; behavior
questions are answered by the spec section cited in each task. New code follows the
existing seams: email behind `domain.EmailProvider`, sends as River jobs, publish as
snapshot rows, all state changes audited.

**Tech Stack:** unchanged (Go 1.25+, pgx/sqlc/goose, River, React+TS+Vite). New: stdlib `net/smtp`+`crypto/tls` (SMTP), stdlib HTTP (Postmark), `golang.org/x/crypto/hkdf` (token subkey — x/crypto is already an indirect dep; promoting it is within the seam rule).

## Global Constraints

- Go 1.25+ floor. Build/test via Makefile: `make test`, `make build`, `make test-integration`.
- **Never log/commit/paste student PII.** Email bodies and regrade bodies are PII: log
  counts/statuses/ids only. Test fixtures use invented names/emails only.
- Third-party libs only at seams. Email = stdlib per spec §3.
- New logic test-first. Unit tests must not require Postgres; integration tests read
  `ADAMARKER_TEST_DATABASE_URL` and skip when unset.
- Don't push to GitHub. Commit locally per task, conventional messages, **explicit file
  paths in `git add` — never `git add -A`** (parallel agents share the tree).
- Migration numbers are assigned here and MUST NOT be renumbered:
  `0016_publish.sql`, `0017_regrade.sql`, `0018_pricing_costcaps.sql`,
  `0019_spot_checks.sql`. Only Task P1 creates migrations or runs `make sqlc`.
- Points/money cross the API as decimal strings, never float64. `cost_usd NUMERIC(10,6)`.
- Status derived not stored (D2); records append-only (D5); official pointer guarded (D6).
- Route handlers follow existing httpapi patterns (per-route body limits per F5,
  `s.audit(...)` on state changes, RBAC middleware).

---

## Milestone P — store foundation (serial; everything else builds on it)

### Task P1: Migrations 0016–0019 + sqlc queries + store methods

**Files:** Create `migrations/0016_publish.sql`, `0017_regrade.sql`,
`0018_pricing_costcaps.sql`, `0019_spot_checks.sql`,
`internal/store/queries/publish.sql`, `internal/store/queries/regrade.sql`,
`internal/store/queries/pricing.sql`, `internal/store/queries/spotcheck.sql`;
Modify `internal/store/queries/audit.sql` (list query exists — expose it),
`internal/store/store.go` (interface additions), regenerate `internal/store/db/`.
Test: extend `internal/store/store_test.go` integration coverage for each new table.

Schema per spec §8 (publish/regrade) and §8 (pricing/spot_checks), including the
pre-existing-runs spot-check waiver backfill (trust spec §4). Add `runs.cost_cap_usd
NUMERIC(10,2)`. Snapshot column is `JSONB`.

**Produces (later tasks consume these exact names):**
- `Store.CreatePublishBatch(ctx, CreatePublishBatchParams) (PublishBatch, []PublishItem, error)` — inserts batch + items in one tx, sets `answers.published_at` for the assessment.
- `Store.PublishPreview(ctx, assessmentID) (PublishPreviewRow, error)` — coverage counts, blockers (answers lacking official), per-student snapshot inputs, changed-vs-latest-batch student ids.
- `Store.SupersedePublishBatch(ctx, batchID, actorID) error` — clears `published_at`, stamps `superseded_at`.
- `Store.ListPublishBatches/ListPublishItems`, `Store.UpdatePublishItemEmailStatus(ctx, itemID, status, providerID, errText) error`, `Store.PublishItemsByStatus(ctx, batchID, status)`.
- `Store.InsertRegradeRequest`, `Store.CountVerifiedRegrades(ctx, studentID, assessmentID) (int, error)`, `Store.ListRegradeRequests(filters)`, `Store.GetRegradeRequest`, `Store.ResolveRegradeRequest`.
- `Store.UpsertModelPricing/ListModelPricing(providerID)`, `Store.MonthToDateCost(ctx) (numeric string, error)`, `Store.RunCost(ctx, runID)`.
- `Store.InsertSpotChecks(ctx, runID, recordIDs)`, `Store.ListSpotChecks(runID)`, `Store.SetSpotCheckVerdict`, `Store.SpotCheckState(ctx, runID) (total, done int, waived bool, err error)`, `Store.WaiveSpotCheck(runID, actorID, reason)`.
- `Store.ListAudit(ctx, ListAuditParams) ([]AuditRow, error)` (filters + limit/offset).

Commit: `feat(store): publish, regrade, pricing, spot-check schema + queries (0016-0019)`.

---

## Milestone Q — email + publish backend

### Task Q1: `internal/email` package (no DB — may run parallel with P1)

**Files:** Create `internal/email/email.go` (constructor + config struct),
`internal/email/file.go`, `internal/email/smtp.go`, `internal/email/postmark.go`,
`internal/email/none.go`, `internal/email/token.go`, `internal/email/template.go`,
`internal/email/testdata/inbound_postmark.json` (invented data);
tests alongside each.

Per spec §3–§4. Token via HKDF subkey from the D16 master key
(`internal/secrets` exposes the key material — add a
`Secrets.Derive(info string) []byte` helper there if absent, test-first).

**Produces:**
- `email.New(cfg email.Config) (domain.EmailProvider, error)` where `email.Config{Provider, From, ReplyDomain, SMTPHost, SMTPPort, SMTPUser, SMTPPass, PostmarkToken string; Rate float64}`.
- `email.MintToken(key []byte, itemID int64, expiry time.Time) string` / `email.VerifyToken(key []byte, tok string, now time.Time) (itemID int64, err error)`.
- `email.RenderGradeEmail(d email.GradeEmailData) (domain.OutboundEmail, error)` with `GradeEmailData{AssessmentName, StudentName string; Problems []ProblemBreakdown; Total, Max string; ReplyTo string; RegradeDeadline time.Time; Corrected bool}` and `ProblemBreakdown{Label string; Criteria []CriterionLine{Name, Score, Max, Comment string}}`.
- `email.RenderRegradeConfirmation(...)`, `email.RenderRegradeResolution(...)` (spec §5–§6).

SMTP tested against an in-process listener asserting STARTTLS negotiation + message
bytes; Postmark against `httptest.Server` asserting auth header + JSON; file provider
asserted on disk; `ParseInbound` on the fixture.

Commit: `feat(email): EmailProvider implementations (file/smtp/postmark/none) + regrade tokens + templates`.

### Task Q2: config + wiring for email

**Files:** Modify `internal/config/config.go`(+test) — `ADAMARKER_EMAIL_PROVIDER`
(default `file` in dev, `none` absent in prod is a loud warning not an error),
`ADAMARKER_EMAIL_FROM`, `ADAMARKER_EMAIL_REPLY_DOMAIN`, `ADAMARKER_SMTP_{HOST,PORT,USER,PASS}`,
`ADAMARKER_POSTMARK_TOKEN`, `ADAMARKER_EMAIL_RATE`, `ADAMARKER_REGRADE_WINDOW`,
`ADAMARKER_REGRADE_MAX`, `ADAMARKER_INBOUND_WEBHOOK_SECRET`,
`ADAMARKER_MONTHLY_BUDGET_USD` (validation per spec §3 + trust spec §3: prod +
provider∈{smtp,postmark} requires From; smtp requires host/user/pass; postmark requires
token; budget/durations parse loudly). Modify `cmd/adamarker/main.go` (build provider,
inject), `.env.adamarker.example`.

**Produces:** `Config.Email email.Config`-shaped fields + `Config.MonthlyBudgetUSD string`, `Config.RegradeWindow time.Duration`, `Config.RegradeMax int`, `Config.InboundWebhookSecret string`.

Commit: `feat(config): email provider + regrade + budget configuration`.

### Task Q3: publish service + endpoints + send jobs

**Files:** Create `internal/publish/publish.go`(+`publish_test.go`) — service over
Store + EmailProvider + Queue; `internal/httpapi/publish.go`(+`publish_test.go`) —
the five routes from spec §7; Modify `internal/httpapi/api.go` (register),
`internal/queue/river.go` (new `email_send` job on an `email` queue, rate-limited,
F17 drain semantics: drain-cancel keeps item `pending`), `internal/grading/manual.go`
or wherever official-pointer writes live (enforce the published lock → 409;
locate via `rg published_at internal/`).

Per spec §2, §3 (pipeline), §7. Snapshot built from the same queries backing
`handleAssessmentTotals` + per-criterion records (reuse, don't duplicate SQL — extend
`PublishPreview` in store if a field is missing). Changed-only re-publish diffs
snapshot JSONB against the student's item in the latest non-superseded batch.
`skipped` for all-no_submission students; `none` provider ⇒ all items skipped +
warning in response. Audit: `publish.create`, `publish.unpublish`,
`publish.resend_failed`.

Integration tests: coverage gate blocks, publish locks official writes (409), snapshot
matches a hand-built fixture, changed-only selection, resend-failed only re-enqueues
failed, unpublish reopens.

Commit: `feat(publish): Phase 6 publish state machine + grade-email send pipeline`.

---

## Milestone R — trust & cost backend

### Task R1: cost population + pricing CRUD + caps

**Files:** Modify `internal/grading/runner.go` (+ wherever records insert — compute
`cost_usd` from tokens × pricing at insert; per-leaf run-cap check ⇒ terminal
`budget_exceeded` failure reason), `internal/httpapi/providers.go`(+test) (pricing
sub-resource: `GET/PUT /api/providers/{id}/pricing`), `internal/httpapi/runs.go`(+test)
(run create: accept `cost_cap_usd`, monthly-budget 409 with `{month_to_date, estimate,
budget}` decimal strings; estimate per trust spec §3 = answers × (1500 in + 400 out) ×
pricing, per model, "unknown" when pricing missing — never fake $0).

Consumes P1 pricing/spend methods. Tests: cost math NUMERIC rounding, cap-stops-leaves
(fake provider), 409 math, estimate with partial pricing.

Commit: `feat(cost): model pricing, cost_usd population, run + monthly budget caps`.

### Task R2: spot-check gate

**Files:** Create `internal/grading/spotcheck.go`(+test) — deterministic seeded,
problem-stratified sample per trust spec §4; Modify `internal/httpapi/runs.go`
(sample created when a run reaches completed; `GET /api/runs/{id}/spot-check`,
`POST /api/runs/{id}/spot-check/{recordID}` `{verdict, note}`,
`POST /api/runs/{id}/spot-check/waive` admin+reason+audit; accept-official 409s until
`SpotCheckState` complete-or-waived), `internal/grading/aggregate_run.go` (same gate on
auto-set-official).

Tests: determinism, stratification, both accept paths gated, waive audited.

Commit: `feat(trust): spot-check gate before bulk accept-official`.

### Task R3: distribution + reports + audit read

**Files:** Modify `internal/store/queries/analysis.sql` + regenerate **only if P1 left
gaps — otherwise queries were made in P1** (coordinate: P1 owns sqlc; if a query is
missing, this task's brief says add it via a P1 follow-up commit by the same P1 agent
pattern — in practice: R3 runs after P1 and MAY run `make sqlc` since P1 is done),
`internal/httpapi/analysis.go`(+test) (`GET /api/problems/{id}/score-distribution`
per trust spec §5; override-rate + cost-per-run additions §7),
`internal/httpapi/api.go` (register + `GET /api/audit` admin-only per §6).

Tests: fixture distributions ⇒ exact stats; sparse-official fallback labeled;
audit RBAC 403, filters, pagination.

Commit: `feat(analysis): score distributions, override rate, run cost, audit read API`.

---

## Milestone S — frontend (after the backend it displays)

Shared: types in `frontend/src/lib/types.ts`, API fns in `frontend/src/lib/api.ts`,
existing component idioms (Cards, Dialog, decimal.ts for money — never parseFloat).

### Task S1: UI stitches (backend exists today — may run parallel with P1)

**Files:** Modify `frontend/src/pages/AssessmentDetail.tsx`: header "Export CSV"
button → `GET /api/assessments/{id}/export.csv` (anchor download, no fetch), and a
"Totals" card fetching `GET /api/assessments/{id}/totals` (student, per-problem,
total; sortable by total; **display only** — no PII in console/logs).

Commit: `feat(ui): export CSV button + assessment totals view`.

### Task S2: Publish tab

**Files:** Create `frontend/src/pages/PublishTab.tsx` (+ register in
`AssessmentDetail.tsx` tab strip): preview (coverage %, blockers list linking to
ProblemReview, skip list, changed-only count, per-problem distribution component from
S3), publish dialog (note, resend-all toggle, typed assessment-name confirm like the
delete guardrail), batch history table (per-item statuses, resend-failed button),
unpublish (admin, typed confirm). Poll batch status while sends are in flight
(follow D27 polling idiom).

Commit: `feat(ui): publish tab — preview, publish, history, resend`.

### Task S3: run cost + spot-check UI + distribution component

**Files:** Create `frontend/src/components/ScoreDistribution.tsx` (10-bucket bar
histogram + mean/σ/%0/%max, pure SVG, no chart lib); Modify
`frontend/src/pages/Runs.tsx`: create-dialog cost-cap field + estimate display
(+month-to-date), run detail cost line, spot-check strip (sample list, image +
AI grades, agree/adjust buttons, progress `done/total`, waive for admins), accept-official
button disabled with reason until gate open; Modify `frontend/src/pages/ReviewTab.tsx`
(embed ScoreDistribution per problem).

Commit: `feat(ui): spot-check flow, cost caps + estimates, score distributions`.

### Task S4: analysis + audit UI

**Files:** Modify `frontend/src/pages/AnalysisTab.tsx` (override rate + mean |Δ| per
method, cost per run/answer), `frontend/src/pages/Users.tsx` (admin-only "Audit"
section: filterable table, newest first, 50/page, collapsed detail JSON).

Commit: `feat(ui): override-rate + cost reports, audit log viewer`.

---

## Milestone T — Phase 7 inbound regrade (after Q3)

### Task T1: webhook + verification ladder + queue API

**Files:** Create `internal/httpapi/regrade.go`(+`regrade_test.go`): webhook route
(path-secret constant-time, size-limited, **no session auth** — register outside the
auth middleware exactly like the OAuth callback), ladder per spec §5 (each rung's
rejection recorded, zero reply for unverified), rate cap via
`CountVerifiedRegrades`, confirmation email enqueue on verified; queue routes
`GET /api/regrades`, `GET /api/regrades/{id}`, `POST /api/regrades/{id}/resolve`
(+ resolution email + audit `regrade.resolve`). Modify `internal/httpapi/api.go`.

Tests: every ladder rung, cap, no-backscatter (assert zero sends on rejects),
resolve→email→audit.

Commit: `feat(regrade): Phase 7 inbound webhook, verification ladder, regrade queue API`.

### Task T2: Regrades UI

**Files:** Create `frontend/src/pages/Regrades.tsx` (+ nav entry in `App.tsx`):
status-filtered queue grouped by assessment, detail pane (email text, published
snapshot, AnswerView link), resolve dialog (uphold/regraded + note, B-H15 lower-grade
warning), "needs re-publish" chip on assessments with post-publish grade changes
(from publish preview's changed-count).

Commit: `feat(ui): regrade queue page`.

---

## Milestone U — ops kit (independent; parallel with Q/R)

### Task U1: CI workflow

**Files:** Create `.github/workflows/ci.yml`: job `go` — Postgres 16 service,
`go vet ./...`, `go test ./... -count=1` with `ADAMARKER_TEST_DATABASE_URL` pointed at
the service; job `frontend` — `npm ci` + `npm run build` in `frontend/` (tsc runs in
the build); job `sqlc` — `go tool sqlc diff` (drift check). Pin action versions.
Note in the workflow header: OCR/live-provider tests self-skip (env-gated).
Verify locally with `uvx --from actionlint-py actionlint` (or download actionlint) —
CI itself can't run tonight (no push).

Commit: `ci: GitHub Actions — go vet+test with Postgres, frontend build, sqlc drift`.

### Task U2: deploy assets + OPERATIONS.md

**Files:** Create `deploy/adamarker.service` (hardened systemd unit,
`TimeoutStopSec=6m` > drain per F17 comment in config.go), `deploy/Caddyfile.example`
(TLS termination → :8080), `deploy/backup.sh` (blobs-then-db order per D15, retention
prune, optional `BACKUP_RSYNC_TARGET` off-host step, writes
`backups/last-backup-ok` timestamp file), `deploy/adamarker-backup.{service,timer}`
(nightly); `docs/OPERATIONS.md`: install steps, TLS, env checklist (from
`.env.adamarker.example`), backup design, **restore procedure** (stop app → restore
blobs tarball → `psql <` dump → start → verify) and a **verification** section using
Task U3's ref-integrity command; RPO statement (nightly ⇒ ≤24h). `shellcheck`-clean
script (`uvx` has no shellcheck — use `shellcheck` if installed, else careful review).

Commit: `ops: systemd/Caddy deploy assets, nightly backup, OPERATIONS runbook`.

### Task U3: ops status + blob ref-integrity check

**Files:** Modify `internal/httpapi/api.go` + create `internal/httpapi/ops.go`(+test):
`GET /api/ops/status` (admin): river job counts by state (query river_job directly),
oldest running job age, blob-dir free bytes (`syscall.Statfs`), DB size, last-backup
timestamp (reads `backups/last-backup-ok` if present). Create
`cmd/adamarker/verify.go`: `adamarker -verify-blobs` flag — walks answer_pages /
publish-era blob refs, reports missing files, exit 1 on any (used by OPERATIONS
restore verification).

Commit: `feat(ops): /api/ops/status + blob ref-integrity verifier`.

---

## Milestone V — close-out (serial, last)

### Task V1: docs sweep

**Files:** Modify `README.md` (Phases 6–7 + trust + ops now built; morning-review
pointers), `docs/DECISIONS.md` (D28–D40 entries from both specs' flags),
`docs/PLAN_GAPS.md` (flip B-C3, B-C5, B-H5, B-H10, B-H14, B-C1-partial, audit-read;
note explicitly-still-open: bounces, retention B-H7, batch API, cross-exam reports),
`docs/MODELS.md` (pricing now lives in Providers UI).

Commit: `docs: close out Phase 6-7 + trust/cost + ops in README/DECISIONS/PLAN_GAPS`.

### Task V2: verification + review (orchestrator-owned)

`make test-integration` green (17+ packages), `make frontend && make build` green,
multi-agent code review over `main...` tonight's range, findings fixed, live email
attempt iff creds exist (one message to b11902156@ntu.edu.tw, invented grade data),
else `file` provider demo + morning instructions. Morning report last.

---

## Execution notes

- Order: P1 ∥ Q1 ∥ S1 ∥ U1 ∥ U2 → Q2 → Q3 ∥ R1 ∥ R2 ∥ R3 ∥ U3 → S2 ∥ S3 ∥ S4 ∥ T1 → T2 → V1 → V2.
- Parallel agents: disjoint file sets are listed per task — respect them; explicit-path
  `git add` only; if two tasks must touch `api.go`/`types.ts`, the later-finishing one
  rebases by re-reading the file before editing (they touch different route blocks).
- Each task's agent reads: this plan's task, both specs, CLAUDE.md, and the files it
  modifies. Reports to `.superpowers/sdd/` per house convention.
