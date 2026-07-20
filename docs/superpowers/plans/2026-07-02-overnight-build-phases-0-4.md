# ADA-Marker Overnight Build (Phases 0–4) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take ADA-Marker from the Phase-0 walking skeleton to a working system: DB + auth + assessments/rubrics/roster + PDF ingestion with masking + browsable manual grading + single-model AI grading runs.

**Architecture:** One Go binary (stdlib mux) + Postgres (pgx/sqlc/goose) + River workers in-process + local-disk blobs, serving an embedded Vite/React SPA. All external deps behind the five seams. Every design choice follows `docs/DECISIONS.md` (D1–D15) and the spec.

**Tech Stack:** Go 1.25+, pgx/v5, sqlc, goose, scs+pgxstore, x/oauth2, River, go-pdfium (wazero WASM), stdlib image/draw, React+TS+Vite+Tailwind+shadcn/ui.

## Global Constraints

- Go 1.25+ floor (River). Build/test via Makefile: `make test`, `make build`, `make run`.
- Python only via `uv` (`uv run`, `uvx`) — never bare python3/pip.
- **Never log/commit/paste student PII** — logging policy D14 (IDs only).
- Third-party libs only at the seams (Renderer, BlobStore, VisionProvider, EmailProvider, Queue) plus the sanctioned infra set above.
- New logic test-first. Unit tests must not require Postgres; integration tests use `ADAMARKER_TEST_DATABASE_URL` and skip when unset.
- Don't push to GitHub. Commit locally per task with conventional messages.
- Points are `NUMERIC(6,2)` / decimal strings — never float64 across API boundaries.
- Status is derived, never stored (D2). Records append-only (D5). Official pointer guarded (D6).

---

## Milestone A — Phase 0 completion (foundation)

### Task A1: Dev infrastructure — docker compose + Makefile + env

**Files:** Create `docker-compose.yml`, `.env.adamarker.example`; Modify `Makefile`, `.gitignore`, `internal/config/config.go`(+test).

- Postgres 16 service `db` (port 5433 to avoid clashes, volume, healthcheck) + `db-test` (port 5434, tmpfs).
- Config gains: `SessionSecret` (required in prod), `GoogleClientID/Secret`, `OAuthRedirectURL`, `BootstrapAdminEmail`, `DevLogin bool`, provider env detection (D11).
- Make targets: `db-up`, `db-down`, `test-integration`, `sqlc`, `frontend`, `dev`.
- Tests: config test-first for new fields (prod requires session secret; dev-login flag only honored in development).

### Task A2: Migrations + store layer (goose + pgx + sqlc)

**Files:** Create `migrations/0001_core.sql` … `0004_grading.sql`, `sqlc.yaml`, `internal/store/queries/*.sql`, `internal/store/store.go` (Pool + RunMigrations + generated code), integration test harness `internal/store/store_test.go`.

Schema per DECISIONS (D1–D6, D12): users, sessions(scs), audit_log; assessments, problems, rubric_versions, rubric_criteria, reference_solution_versions, students; submissions (superseded_by, sha256, active-unique partial index), answers (natural key unique, official_record_id + official lock columns, published_at, flags text[]), answer_pages (ordered, refs+sha), mask_regions, mask_page_reviews; prompt_template_versions, grading_methods, grading_method_versions, grading_runs, grading_run_items, grading_records (+ deferred FK answers.official_record_id).

**Produces:** `store.New(ctx, dsn) (*Store, error)`, `store.RunMigrations(ctx, pool)`, sqlc-generated typed queries used by all later tasks.

### Task A3: OAuth + sessions + RBAC middleware + bootstrap

**Files:** Create `internal/auth/oauth.go`(+test: state/PKCE/nonce round-trip, callback rejection cases), `internal/auth/session.go`, `internal/httpapi/api.go` (router skeleton, /api/me, middleware: RequireUser, RequireRole, CSRF header check), `internal/auth/bootstrap.go`(+test); Modify `cmd/adamarker/main.go` to wire DB→sessions→router.

Flow per D7; dev-login per D8; `/readyz` checks DB ping. Google token verify via JWKS (google idtoken lib or manual with x/oauth2 + jws — use `google.golang.org/api/idtoken`).

### Task A4: Frontend scaffold + embed wiring

**Files:** Create `frontend/` (Vite React-TS, Tailwind v4, shadcn/ui, react-router, TanStack Query), `frontend/vite.config.ts` (outDir `../internal/web/assets/dist`, proxy /api+/auth+/readyz), login page + authenticated shell (nav: Assessments, Students, Methods, Runs, Users) + /api/me guard; Modify `internal/web/web.go` (serve dist when embedded, D9), `Makefile`, `.gitignore`.

Verify: `make frontend && make build` produces a binary serving the real SPA; `make dev` = Go + Vite dev proxy.

---

## Milestone B — Phase 1 (assessments, rubrics, roster)

### Task B1: Assessments + problems API & guardrails

`internal/httpapi/assessments.go`(+tests via httptest against a store fake or test DB): CRUD, archive (soft), hard-delete blocked when submissions/records exist unless admin `force` + type-name confirmation (D-guardrails, audit-logged). Problems nested CRUD (`number` unique per assessment, `position` for ingest order, `max_points` numeric string).

### Task B2: Rubrics + reference solutions (versioned)

`internal/httpapi/rubrics.go`(+tests): GET latest+history, POST creates version N+1 (insert-only, D5), Σ criteria == max_points invariant (D4) → 400 with per-criterion detail. Same pattern for reference solutions.

### Task B3: Roster CSV import

`internal/roster/csv.go`(+unit tests: happy, BOM, missing column, dup student_id, upsert semantics report) per D13; `POST /api/students/import` (multipart), `GET /api/students`.

### Task B4: Phase-1 frontend

Pages: Assessments list/create/archive; Assessment detail (problems editor, rubric editor with criteria rows + live Σ check, reference solutions); Students page with CSV upload + import report. Commit per page group.

---

## Milestone C — Phase 2 (ingestion + masking)

### Task C1: BlobStore local-disk impl

`internal/blobstore/localdisk.go`(+unit tests incl. path traversal rejection, atomic write via temp+rename, sha256 helper). Keys: `assessments/{id}/submissions/{sid}.pdf`, `answers/{id}/pages/{n}.jpg`, `.../masked/{n}.jpg`.

### Task C2: Renderer — go-pdfium WASM

`internal/render/pdfium.go`(+test rendering a tiny generated PDF; check dims/DPI/downscale cap). Interface (revised A3/D1): `Render(ctx, pdf []byte, pageIndex int, opts) (RenderedPage, error)` + `PageCount(ctx, pdf []byte) (int, error)`. Pool wazero instances (they're not goroutine-safe); config DPI default 250, MaxLongEdgePx default 2200.

### Task C3: Ingestion service

`internal/ingest/ingest.go`(+tests with fake renderer/blobstore + test DB): upload PDFs (multipart, filename `<student_id>.pdf` → roster match, else quarantine list D13); pre-materialize answers for all rostered students (D1); default mapping: page i → problem at position i; store rendered JPGs per page; re-upload semantics per D1 (block-if-graded unless force → flag `image_superseded`). Endpoints: POST submissions, GET mapping report (matched/quarantined/missing/page-count-mismatch), POST mapping corrections (reassign page→answer, mark blank).

### Task C4: Masking

`internal/imaging/mask.go`(+unit tests: draws rect, clamps out-of-bounds, padding, idempotent bytes, `MaskedImage` type constructible only here): normalized rects → pixel rects → `draw.Draw` uniform color → jpeg q85 (D10). Mask regions API (GET/PUT per assessment) + apply-masks action (renders masked derivative for every answer page; records `masked_at`) + masked-crop review endpoints (accept/flag per page, D10).

### Task C5: Image streaming + Phase-2 frontend

Authenticated streaming: `GET /api/answers/{id}/pages/{n}/image?variant=original|masked`, `GET /api/submissions/{id}/pdf` (D10). Frontend: upload screen with per-file results; mapping review table; mask-region selector (drag rect over page 1 preview, normalized save, react-rnd or hand-rolled); masked-crop review screen (keyboard j/k + a/f).

---

## Milestone D — Phase 3 (drill-down + manual grading)

### Task D1: Drill-down read APIs

`internal/httpapi/review.go`: problem list w/ per-problem count summaries (D2 rollups); student list per problem (name, email, official total, derived status, flags); answer detail (pages, records history w/ versions-as-graded, official pointer, flags). SQL does the derivation; tests against test DB.

### Task D2: Manual grading + official pointer

`internal/grading/manual.go`(+tests: snap/clamp per D4, total computed app-side, human record insert, official set w/ optimistic concurrency + human lock per D6, audit rows): `POST /api/answers/{id}/records` (per-criterion scores vs pinned rubric version), `POST /api/answers/{id}/official {record_id, expected_record_id}`.

### Task D3: Phase-3 frontend

Problem list → student list → **Answer view**: page images (zoom), rubric panel with per-criterion inputs (increment-aware steppers), records history with diff-style comparison, official badge + set-official, flag toggle, keyboard next/prev + next-ungraded (B-M18). This is the biggest UI task; commit in slices.

---

## Milestone E — Phase 4 (methods, runs, AI grading)

### Task E1: Grading methods + prompt templates

Seed a default prompt template (transcribe-then-grade, per-criterion JSON) + default method at migration/startup; methods CRUD with versioning (config JSONB: provider, model, temperature default 0, reasoning level, ref-solution count, reask_cap 2, rubric+prompt+refsol version pins per D5). API + minimal frontend editor.

### Task E2: VisionProvider — anthropiccompat + fake

`internal/llm/anthropiccompat/`(+tests with httptest server: request shape, image blocks, tool-forced JSON schema output, usage parse, 429 Retry-After surfaced) — Anthropic Messages API against configurable base URL (DeepSeek/Qwen per D11); constrained JSON via forced tool use (`tool_choice`), fallback to prompt+extract. `internal/llm/fake/` deterministic provider. Provider registry from config env.

### Task E3: Queue + run execution

`internal/queue/river.go` + `internal/grading/run.go`(+tests using fake provider + test DB): launch (tx: insert run + PlanRun job); planner resolves scope → items + enqueues leaves (InsertManyFast, unique by item); leaf worker: load masked pages (MaskedImage only, fail `mask_missing`), build prompt from frozen versions, rate-limit per provider (`x/time/rate`), call, validate/snap/clamp, re-ask ≤ cap, write record + item state (idempotent: skip if record exists); refusal→`confidence=illegible` record + flag (D12); run status derivation + cancel + retry-failed. Mask-review gate at launch (D10).

### Task E4: Runs API + frontend + bulk accept

POST /api/runs (scope+method, pre-flight leaf count), GET /api/runs + /{id} (item breakdown), cancel, retry-failed; **bulk accept-official** `POST /api/runs/{id}/accept-official {only_unflagged:true}` obeying D6 locks, audited (B-C9). Frontend: launch dialog (scope picker, method picker, count preview), runs list w/ polling progress, per-run item table with errors; answer-view already shows records for comparison.

### Task E5 (stretch): grades CSV export (B-C11)

`GET /api/assessments/{id}/export.csv` — student_id, name, per-problem official totals, assessment total, source, flags. Streamed, no PII in logs.

---

## Verification (every milestone)

`make test` green; `make vet`; integration tests against dockerized Postgres; `make frontend && make build` and smoke the flows by running the binary + curl / browser-level checks. Final wrap-up per session Task #7.
