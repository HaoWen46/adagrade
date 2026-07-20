# ADA-Marker — Architecture & Framework Design Spec

*Date: 2026-07-01 · Status: draft for review · Companion to `ADA-Marker_Plan.md` (the product source-of-truth)*

This spec fixes the **how**: the concrete stack, module boundaries, data model, and core
runtime flows. It defers to `ADA-Marker_Plan.md` for the **why/what** and keeps the plan's
objectives and domain shape intact. Empirical choices below (models, licenses, pricing) were
researched and independently fact-checked in mid-2026; material corrections are noted inline.

---

## 1. Confirmed constraints (drivers for every decision here)

| Driver | Value | Consequence |
|---|---|---|
| Backend language | **Go** (solo maintainer) | Single-binary, no polyglot ops. |
| Deployment | **Self-hosted, single on-prem/university VM** | No Redis, no managed queue, no S3. Postgres + files on the box. |
| Scale | **50–200 students/assessment**, ~3 exams + ~3 assignments/term, ~6–10 problems each | A run ≈ 1,000–3,000 vision-LLM calls → durable async + rate limiting required; distributed infra is not. |
| Privacy | **Physical masking**: TA selects a fixed name-region; a solid rectangle is drawn on the *copy* sent to the model; original untouched on disk | New preprocessing component; identity never reaches the model. |
| Grading mode (decided) | **Batch API by default for multi-answer runs; synchronous for single-answer / interactive re-grades** | Grading orchestration supports two execution paths writing the same records. |
| v1 rigor (decided) | **Single-model default method + multi-model agreement (Phase 5)**; confidence-triggered auto-escalation deferred | Method config already carries `agreement_rule` + confidence so escalation is additive later. |

---

## 2. Tech stack (the framework decision)

Everything runs as **one Go binary + PostgreSQL + local disk** on a single VM, with the React
SPA embedded in the binary. Each external dependency sits behind a swappable interface.

| Concern | Choice | Rationale / verified caveat |
|---|---|---|
| HTTP router | **stdlib `net/http` ServeMux (Go 1.22+ patterns)** | Native method+path routing; `chi` is a cheap drop-in later if route-group middleware is needed. |
| Auth | **Hand-rolled Google OAuth (`golang.org/x/oauth2/google`)** verifying **`aud` + `hd`** claims **+ DB allowlist**; sessions via **`alexedwards/scs` + `pgxstore`**; cookies `Secure/HttpOnly/SameSite=Lax` | The allowlist is the real gate (`hd` alone fails if staff use personal Gmail). Server-side sessions = instant revocation. scs `pgxstore` auto-cleans expired rows every 5 min. |
| DB access | **`pgx/v5` + `sqlc`** (compile-time-checked SQL) + **`goose`** migrations (embedded, run in-process at startup) | Query/schema drift becomes a build error — right safety net for a solo dev. pgx native JSONB + LISTEN/NOTIFY. |
| Job queue | **River** (`github.com/riverqueue/river`, MPL-2.0, free tier) via `riverpgxv5`, same Postgres | Transactional enqueue (no "record written, job lost"). **Correction:** use **`InsertManyFast`/`InsertManyFastTx`** for the COPY fan-out — plain `InsertMany` no longer uses COPY. Requires **Go 1.25+**. River Pro ($125/mo) **not** needed. |
| Per-provider throttling | **One River queue per provider (fixed `MaxWorkers`) + `golang.org/x/time/rate` token bucket keyed by provider** (`Limiter.Wait(ctx)`) | Gives both a concurrency ceiling and a request-rate ceiling on the free tier. Keep worker-process count = **1** so the limiter is exact. |
| PDF split + render | **`klippa-app/go-pdfium` in WASM (wazero) mode** primary; **Poppler `pdftoppm`** subprocess fallback; **`pdfcpu`** for split/validate only | Permissive end-to-end (go-pdfium MIT, PDFium + bblanchon binaries Apache-2.0), no cgo, crash-isolated. **`go-fitz`/MuPDF ruled out**: AGPL-3.0 network clause would force open-sourcing the whole app. `pdfcpu` is **not** a rasterizer. Optional cgo + `pdfium_use_turbojpeg` build tag later if raster throughput ever matters. |
| Image masking | **Go stdlib** `image` + `image/jpeg` + `image/draw`: decode → `*image.RGBA` → `draw.Draw` solid rect (`draw.Src`) → `jpeg.Encode` | Zero deps, static binary. libvips/ImageMagick drag in cgo/subprocess for no benefit on one opaque rectangle. |
| Vision LLM | **Provider-agnostic `VisionProvider` interface**; default = **Gemini Flash-class** (pin a GA id, e.g. `gemini-2.5-flash`, in config); **Claude Sonnet 5** + **GPT-5.4** wired as alternates; **Opus 4.8 / GPT-5.5** as arbiter/premium presets | Flash is the price/quality sweet spot for 1–3k calls/run. **Correction:** Gemini vision billing is **not** a flat ~560 tok/image (that's the image-*gen* model) — it's 258 tok ≤384px else 258/768px-tile, so **downscale scans to smallest-legible size** (real cost lever). **Do not** default to GPT-5.6 Sol/Terra/Luna (gated preview, not GA). |
| Structured output | Provider-native **constrained decoding** (Gemini `responseJsonSchema`, Anthropic `output_config.format` json_schema, OpenAI strict) + **app-side re-validation** against the run's frozen rubric schema + **bounded re-ask (max 2)** | ~99.8–99.9% conformance is not 100%, and vision+strict is least-tested. **Score-range clamping lives in app code** — strict schemas drop numeric `minimum`/`maximum`. Store raw response alongside parsed for audit. |
| Email | **Postmark Pro** ($16.50/mo, inbound+outbound share a 10k/mo pool) behind an `EmailProvider` interface; **Mailgun** as fallback | Cleanest inbound-parse JSON + turnkey deliverability. **Postmark does not HMAC-sign inbound** → trust = *signed reply token* + sender/SPF/DKIM check + Basic-Auth-in-URL & IP-allowlist on the webhook. |
| Frontend | **React + TypeScript + Tailwind + shadcn/ui**, built with **Vite**, **`go:embed`-ed** into the binary, SPA `index.html` fallback | One deploy artifact, same-origin cookie auth, no CORS/nginx. `react-rnd` (MIT) for the mask-region selector. |
| Storage | **Local disk** behind a `BlobStore` interface (source PDFs, rendered JPGs, masked JPGs) | Swappable to object storage later without touching callers. |

**Cost sanity check:** a full run on a Flash-class model via Batch API (~1.5k in / 400 out per
call, 1–3k calls) is **single-digit dollars**; Sonnet/Opus arbitration adds low tens only for
the flagged minority.

---

## 3. Module architecture & the five seams

Swappability + testability hinge on five interfaces: **`Renderer`** (PDF→JPG), **`BlobStore`**
(files), **`VisionProvider`** (LLM, with an optional batch sub-capability), **`EmailProvider`**
(send + inbound), **`Queue`** (enqueue/plan/cancel/pause/status).

```
cmd/adamarker/         main: config, migrations, HTTP server, River workers — one process
internal/
  config/              env + secrets loading
  httpapi/             handlers, routing, middleware (auth, RBAC), SSE run-status
  auth/                Google OAuth flow, scs sessions, allowlist, roles
  store/               pgx + sqlc-generated queries; repositories per aggregate
  domain/              core types + invariants (no I/O)
  blobstore/           BlobStore iface + localdisk impl
  render/              Renderer iface + gopdfium(WASM) / pdftoppm impls
  ingest/              split → render → page→(student,problem) mapping + manual correction
  imaging/             redaction masking (normalized-rect → masked derivative)
  llm/                 VisionProvider iface + google/anthropic/openai adapters (sync + batch)
  grading/             method resolution, run planner, grade worker, agreement, validation
  queue/               River setup, job kinds, Queue-iface wrapper
  email/               EmailProvider iface + postmark impl; token mint/verify; regrade workflow
  audit/               append-only audit log
  web/                 go:embed of built assets + SPA serving
frontend/              React/TS/Vite/Tailwind/shadcn
migrations/            goose SQL
```

Each unit answers: *what it does / how you use it / what it depends on.* Vendors (PDFium,
Postmark, Gemini, River, disk) are reachable only through their seam, so any one can be replaced
without touching consumers.

---

## 4. Domain model → PostgreSQL schema shape

Relationships mirror `ADA-Marker_Plan.md` §3. Tables (relationships > exact columns):

- `users` — email, role (`admin`/`lecturer`/`ta`), active; the auth allowlist.
- `assessments` — type (`exam`/`assignment`), name, status, `archived_at` (soft-delete).
- `problems` — assessment_id, number, max_points, order.
- `reference_solutions` — problem_id, content, version.
- `rubrics` **(versioned)** → `rubric_criteria` — description, points, partial-credit notes, order.
- `students` — name, student_id, email (roster).
- `submissions` — assessment_id, student_id, source_pdf_ref, uploaded_at.
- `answers` — the atomic gradable unit `(student, problem)`: `source_page_ref`, `image_ref`,
  `masked_image_ref`, `status`, **`official_record_id`** (FK → the published record).
- `grading_methods` **(versioned)** — config JSONB: model(s)+provider, prompt_template ref,
  reasoning level, #reference solutions, agreement rule, execution mode, rubric version, re-ask cap.
- `prompt_templates` **(versioned, immutable)**.
- `grading_runs` — scope (`answer`/`problem`/`assessment`) + scope_ref, grading_method_id,
  rubric_version, status (`pending`/`running`/`paused`/`cancelled`/`completed`/`failed`), counts,
  execution_mode, created_by.
- `grading_records` **(append-only, immutable)** — answer_id, run_id (nullable for manual),
  method+prompt+rubric versions, `source` (`model`/`human`), model_id, per-criterion scores
  (JSONB), total, comment, raw_output (JSONB), confidence signals, tokens, cost, created_by.
- `regrade_requests` — answer_id, student_id, status, count, escalated, token_nonce.
- `mask_regions` — per assessment/page-layout: list of **normalized** rects (fractions 0..1) +
  color/padding.
- `audit_log` — actor, action, target, detail (JSONB), timestamp (create/delete/override/publish).
- Plus River's job tables and scs's session table (their own migrations).

**Two load-bearing rules:**
1. **Official grade is a pointer** (`answers.official_record_id`), never a mutation. A human grade
   is just a `grading_records` row with `source='human'` in the same history.
2. **Versioning is the safety mechanism.** Editing a rubric/method/prompt creates a *new version*;
   records keep referencing the exact version they used → every grade is reproducible (plan §10).

---

## 5. Grading: methods, runs, records (config-as-data)

Grading is parameterized by an **editable `GradingMethod`** and executed as **runs whose records
are kept forever** — never a code edit to try a new approach. Method config (all data): model(s)
+ provider, prompt template version, reasoning level (abstract `off/low/medium/high` mapped
per-provider), #reference solutions, multi-model agreement rule, rubric version, execution mode,
re-ask cap. A sensible **default method** ships so grading works out of the box.

**Per-criterion, transcribe-then-grade prompt.** The model returns, via constrained JSON schema:
(a) a verbatim transcription (LaTeX for math/pseudocode) + a **legibility/confidence flag with an
explicit "illegible/uncertain" path** (active refusal beats hallucinated reads), then (b) per
criterion a score + rationale. Malformed/invalid output is re-asked up to the cap; scores are
**clamped in Go** to `[0, criterion.max]`. Everything (method, versions, model, raw output,
confidence, tokens, cost) is recorded.

**Reliability principles are method-supported, not a single hardcoded flow:** per-criterion
scoring; **fresh session per (answer, model)** so no answer leaks into another's grading;
multi-model cross-check as *one* configurable strategy; confidence/legibility flags drive human
flagging regardless of score.

---

## 6. Execution model: batch-default fan-out + rate limiting

A `grading_runs` row is the source of truth; jobs reference `run_id`.

1. **Launch** (transactional): TA picks scope + method → insert `grading_runs` (`pending`) **and**
   enqueue one `PlanRun` job, atomically.
2. **Plan** (`PlanRun` worker): resolve scope → concrete `(answer, model)` set (a method may name
   several models). Choose **execution mode**: `auto` → **Batch** when the set exceeds a threshold,
   else **sync** (single-answer/interactive always sync). Fan out with `InsertManyFast`, idempotent
   via `UniqueOpts(run_id, answer_id, model)`; run → `running`.
   - **Batch path:** group items into provider **Batch** submissions (Gemini Batch / Anthropic
     Message Batches / OpenAI Batch, behind the `VisionProvider` batch capability; providers
     lacking batch fall back to sync). A recurring `PollBatch` job checks status; on completion,
     ingest each result → validate/clamp → write records. 50% cheaper, tolerates latency.
   - **Sync path:** one `GradeAnswer(answer, model)` leaf per pair, routed to that model's provider
     queue; the worker token-bucket-waits, calls the provider, validates/clamps, re-asks on
     malformed, writes one record. Interactive.
3. **Records:** each success writes exactly one **immutable** `grading_records`; the worker checks
   for an existing record for `(run_id, answer_id, model)` first, so retries are safe. Nothing is
   overwritten — full history preserved.
4. **Agreement (Phase 5):** multi-model methods add a `Reconcile` step applying the agreement rule
   → auto-set official grade on agreement, else flag for human. (Confidence auto-escalation to a
   stronger model is deferred; the schema already supports adding it.)
5. **Progress / control:** progress is **derived from a `GROUP BY state`** over leaf jobs/records
   (restart-safe), streamed to the UI via SSE (`/runs/:id/status`). Pause = pause provider queues
   (drain in-flight); cancel = flip run status + `JobCancel`; a leaf starting post-cancel no-ops.

**Rate limiting:** per-provider request rate (RPS) + burst + `MaxWorkers` are editable config; the
worker blocks on the provider's token bucket before each call, honoring `ctx` cancellation. Respect
`Retry-After` on 429; River's default backoff is `attempts^4 + ~10% jitter`.

---

## 7. Ingestion & masking pipeline

- **Upload → split → render.** Accept student PDFs; split with `pdfcpu`; render each page to JPG at
  a **configurable DPI** (200–300 for handwriting) via the `Renderer` (go-pdfium WASM). Keep both
  the source PDF page and the rendered JPG on each `Answer`. Enforce a downscale/pixel cap so high
  DPI doesn't exceed vision-model image limits (also a cost lever). Normalize orientation at render
  time; never trust EXIF.
- **Map page → `(student, problem)`.** Common case: one PDF/student, one problem/page, known order.
  Support a per-assessment **page-layout config** (positional mapping) + a **filename convention**
  (e.g. `<studentID>.pdf`) for identity. Provide a **manual mapping-correction UI** for the messy
  cases (blanks, skipped/spanning/misordered pages).
- **Masking (redaction).** Store the name-region as **normalized (0..1) rects** per assessment/
  page-layout (DPI-independent). At mask time: decode → RGBA → `draw.Draw` each rect with
  `image.NewUniform(color)` + `draw.Src`, clamped via `rect.Intersect(bounds)` → `jpeg.Encode`
  (quality ~85). The masked JPG is a **derived, idempotent artifact** (pure function of original +
  region); the original is never mutated. **Only the masked JPG is ever sent to a provider;** humans
  always see the original. Configurable: mask color (swappable — some models over-attend to pure
  black), padding to absorb scan drift, multiple regions (name + ID box), quality.
- **Frontend region selection.** `react-rnd` draggable rectangle over the displayed page; map
  displayed→natural pixels via `naturalWidth/clientWidth`; **avoid `object-fit:contain`
  letterboxing**; save as normalized fractions; re-overlay to validate before commit. A TA preview
  of the masked copy per assessment is the privacy safety-check.

---

## 8. Web UI — browsable drill-down + human grading

Full browsing, not a flag-only queue: any submission is clickable; flagged rows are highlighted.
Navigation: **Assessment picker → Problem list (status + progress) → Student list (name, email,
score, status/flags; clickable) → Answer view.**

The **Answer view** is the grading heart: rendered JPG (source PDF available) alongside AI grade(s)
+ comment(s), the full **grading history** (all runs/records, comparable, official one indicated),
the **regrade history**, and per-criterion **grade/override** controls that create a new record and
can set the official grade. Plus **method/rubric/reference-solution management** screens and
**run-launching** controls (pick scope + method → run). Grading problem-by-problem across students
keeps the standard consistent within a problem.

---

## 9. Email — outbound + inbound regrade (two-way, forgery-resistant)

- **Outbound:** individually addressed emails (no shared recipient lists) with total, per-criterion
  breakdown, comments. Reply-to = `regrade+<signed-token>@inbound.<domain>` where
  `token = base64url(payload) . base64url(HMAC-SHA256(server_secret, payload))`,
  `payload = {run_id, answer_id, student_id, roster_email, issued_at, nonce}`. Rate-limit outbound
  with a token bucket in front of Postmark's batch endpoint (≤500/req); retry 429 with backoff.
- **Inbound:** Postmark POSTs parsed JSON to the webhook → recover token from `MailboxHash`
  (fallback `OriginalRecipient`) → **verify HMAC + expiry** → **`FromFull.Email` (normalized) ==
  `token.roster_email`** **and** SPF/DKIM verdicts from the headers array → create `RegradeRequest`
  → apply policy (auto re-grade with a method and/or queue for a human) → increment count. **At 3
  strikes on one answer: force mandatory human review, disable further auto-regrades.** Send an
  automated confirmation on receipt and on resolution. Harden the endpoint with Basic-Auth-in-URL +
  Postmark IP allowlist + HTTPS (Postmark does not HMAC-sign inbound; the signed token + sender
  checks are the trust anchors). Mismatches route to human review, never silent grading.
- **Deliverability:** SPF passes via Postmark's Return-Path; add DKIM TXT + `pm_bounces` CNAME for
  DMARC alignment.

---

## 10. Access control & data-safety guardrails

- **Auth:** Google Workspace sign-in restricted to the DB allowlist; roles `Admin`/`Lecturer`
  (full incl. managing assessments/methods/users/publishing) and `TA` (grade/override, methods,
  regrades). Unlisted emails denied entirely. Secure http-only session cookies.
- **Guardrails (plan §10):** archive/soft-delete by default; **block hard-delete** of any
  assessment/problem with submissions or records unless an Admin explicitly forces it; **require
  type-the-name confirmation** for destructive actions (Admin/Lecturer only); a re-grade or regrade
  **never silently overwrites** a human-finalized/official grade — it's a new record; **audit-log**
  create/delete/override/publish + who; **rubric change after grading → new version**, never a
  mutation of existing records.

---

## 11. Build phases (usable early; AI layers on top)

- **Phase 0 — Skeleton.** `git init` + remote `git@github.com:HaoWen46/adagrade.git`; single-binary
  Go service with `go:embed` shell; Postgres + goose migrations; Google OAuth + allowlist + roles;
  authenticated shell.
- **Phase 1 — Assessments & rubrics.** Create exams/assignments (guardrails), problems, versioned
  rubrics (criteria + points), reference solutions; import roster from CSV.
- **Phase 2 — Ingestion + masking.** Upload PDFs, split + render JPGs, page→`(student, problem)`
  mapping + manual-correction UI, mask-region selection, store PDF + JPG + masked JPG per answer.
- **Phase 3 — Browsable review + manual grading.** Full drill-down + answer view with per-criterion
  manual grading/override. **Usable with zero AI — key de-risking milestone.**
- **Phase 4 — Grading methods & runs (single model).** Editable `GradingMethod`; launch a run over a
  scope; batch/sync execution; records into history; view/compare; set official grade.
- **Phase 5 — Multi-model cross-check.** A method that runs 2–3 models with a configurable agreement
  rule; auto-accept on agreement, flag disagreement/low-confidence.
- **Phase 6 — Publish + outbound email.** Finalize/publish; email breakdowns; embed regrade tokens.
- **Phase 7 — Inbound regrade.** Webhook, token correlation, sender verification, RegradeRequest
  workflow, escalation, confirmations.
- **Phase 8 — Iteration tooling.** Reports over records: method/model agreement, human-override rate,
  cost per run, cross-method/exam comparisons.

Each phase leaves the system working and demoable.

---

## 12. Operational prerequisites (line up before the relevant phase; not blocking design)

- A **domain** for email + DNS access (Postmark SPF/DKIM/DMARC, `pm_bounces` CNAME, inbound MX).
- A **Google Cloud OAuth client ID** for the university Workspace; the `hd` domain (e.g. `ntu.edu.tw`).
- **Vision API keys** for at least the default Gemini provider (Vertex for region/no-train controls)
  + one alternate; confirm the data-handling policy the provider layer must enforce.
- A **VM** with Go 1.25+, PostgreSQL, and `poppler-utils` (only if the pdftoppm fallback is enabled).

---

## 13. Deferred / open questions (from plan §12, do not block v1)

- Exact submission format & collection method (refines §7 mapping) — informs Phase 2 defaults.
- Which models are permitted/affordable and any institutional data-handling constraints beyond
  masking — the provider layer enforces whatever policy lands.
- Regrade policy specifics (auto re-grade vs straight-to-human; threshold) — default 3-strikes.
- Whether assignments are used to calibrate methods before exams.
- Confidence-triggered **auto-escalation** to a stronger model (deferred past v1).

---

## Appendix — research provenance

Six decision areas were each researched with web sources and independently fact-checked; key
corrections applied above: Gemini image-token billing (not flat 560), River `InsertManyFast` for
COPY fan-out, Postmark inbound not HMAC-signed, app-side score clamping under strict schemas,
`go-fitz`/MuPDF AGPL exclusion. Primary sources include vendor pricing/structured-output/rate-limit
docs (Google, Anthropic, OpenAI), Artifex/MuPDF & Poppler licensing, `klippa-app/go-pdfium` &
`bblanchon/pdfium-binaries`, Postmark/Mailgun/SES docs, River docs & `riverqueue.com/pro`, and
Go stdlib `image/draw` / `golang.org/x/time/rate` / `alexedwards/scs` references. Full URL list is
retained in the research workflow output for this project.
