# AdaGrade

AI-assisted grading for handwritten university exams & assignments — the successor to the
course's *Ada Judge* autograder. Formerly developed as *ADA-Marker*; older docs and design
records use that name. A single self-hosted Go binary that ingests handwritten
PDFs, grades them per-rubric-criterion with vision LLMs (against a masked copy that hides
student identity), keeps full grading history, supports human review/override, and handles
grade distribution + regrades entirely over email.

## Start Here

| Need | Read |
|---|---|
| Product scope | [`ADA-Marker_Plan.md`](ADA-Marker_Plan.md) |
| Architecture | [`docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md`](docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md) |
| Portable TA deployment | [`docs/TA_DEPLOYMENT.md`](docs/TA_DEPLOYMENT.md) |
| VM operations, backup, restore | [`docs/OPERATIONS.md`](docs/OPERATIONS.md) |
| Build decisions and caveats | [`docs/DECISIONS.md`](docs/DECISIONS.md), [`docs/PLAN_GAPS.md`](docs/PLAN_GAPS.md) |
| Email, regrade, trust, cost designs | [`publish + regrade`](docs/superpowers/specs/2026-07-03-publish-email-regrade-design.md), [`trust + cost`](docs/superpowers/specs/2026-07-03-trust-cost-design.md) |

## Runtime Shape

- **App:** one Go process serving the API, River workers, migrations, and the embedded SPA.
- **State:** Postgres 16 plus a local blob directory for PDFs/images/reports.
- **Auth:** allowlisted users log in with 15-minute single-use email links; Google OAuth is optional.
- **Email:** file, SMTP, Postmark, or none providers; Postmark is required for inbound regrade replies.
- **Deployment:** bind the app to localhost and expose it through Caddy or Cloudflare Tunnel.

## Implementation Status

Built so far:

- **Phase 0** — Postgres + goose migrations (in-process), email magic-link login
  (one-time 15-minute links) and optional hardened Google OAuth (state/PKCE/nonce)
  behind the same DB allowlist + scs sessions, RBAC, bootstrap admin, dev login,
  embedded React SPA.
- **Phase 1** — assessments/problems CRUD with plan-§10 guardrails, versioned rubrics
  (Σ criteria == max invariant) + reference solutions, roster CSV import.
- **Phase 2** — PDF upload with `<student_id>.pdf` matching + quarantine, PDFium(WASM)
  rendering, answers pre-materialized per roster, mask-region editor + derived masked
  artifacts + per-page mask review gate, authenticated image/PDF streaming.
- **Phase 3** — assessment → problem → student → answer drill-down (statuses derived,
  never stored), per-criterion manual grading with exact snap/clamp math, guarded
  official-grade pointer (human decisions are never auto-overridden).
- **Phase 4** — grading methods as versioned config-as-data, River-backed runs
  (transactional enqueue, per-provider queues + rate limits), transcribe-then-grade
  constrained JSON via Anthropic-compatible providers (DeepSeek/Qwen; validated live
  against `qwen3-vl-plus`), re-ask cap, illegible-refusal path, retry-failed-only,
  bulk accept-official, grades CSV export.
- **Scan intake + identification** — page-level staged upload of giant multi-page scans
  (PDF/zip-of-images), async render/crop/OCR of per-page student-ID/name/problem header
  regions against the roster, auto-assign on independent ID+name agreement with an
  orphan queue + duplicate/conflict parking for everything else, an assessment-wide
  student × problem assignment matrix, and incremental finalize through the per-problem
  image ingest seam (D18, D22–D23, D63–D68).
- **Phase 6** — publish state machine (per-assessment snapshot batches, 100%-coverage
  gate incl. fail-closed `not_ingested` blocker, single-live-batch guard, official-grade
  lock while published, admin unpublish, changed-only re-publish), outbound email
  (`internal/email`: file/smtp/postmark/none providers, HMAC/HKDF regrade tokens,
  text+HTML templates) sent via a River `email` queue with drain-safe shutdown (F17),
  PublishTab UI.
- **Phase 7** — inbound regrade webhook with a 5-rung verification ladder (token →
  batch-live → sender-matches-roster → SPF/DKIM warn-not-block → rate cap), MessageID
  idempotency against webhook retries, no-backscatter on rejects, a regrade queue API +
  atomic resolve, Regrades UI.
- **Report attachments + regrade assist** — per-student result PDF (`internal/report`:
  A4 landscape, original page left / grading panel right, paginated onto continuation
  pages, Noto Sans TC via `make report-fonts`, feature-gated on `ADAMARKER_REPORT_FONT`)
  with a ZIP-of-images fallback, plumbed through publish as three attachment quality
  options (none/compressed/original) + a ZIP checkbox and into email as
  `multipart/mixed`/Postmark attachments; per-item resend
  (`POST /api/publish/items/{id}/resend`, lecturer+); and a stricter AI re-grade
  assist (masked images, redacted request text, its own `regrade_strict` policy and
  `regrade_ai` record source) the TA triggers per-request or batch-wide with a
  dry-run cost estimate — students can never trigger it themselves and it is never
  auto-official.
- **Regrade v2** — regrade is now multi-problem per reply: students name every
  contested problem in one email using a strict `<pN>…</pN>` tag format, each
  problem gets its own AI-assist + TA verdict, the result email is TA-clicked and
  gated until every problem is verdicted, and after `ADAMARKER_REGRADE_MAX` turns
  the thread hands off person-to-person to each problem's assigned TA
  (migration `0025_regrade_v2.sql`; see DECISIONS.md D54–D62).
- **Trust & cost** — per-model pricing (Providers page) drives `cost_usd` at record
  insert; per-run cost caps + a monthly budget 409 with pre-flight estimates; a
  deterministic spot-check gate (canonical first sample, stratified by problem) blocks
  bulk accept-official until a human has sampled the run, with an admin waive escape
  hatch; score-distribution histograms (official, or AI-fallback when officials are
  sparse — labeled); override-rate and cost reports; an audit-log read API + UI.
- **Ops kit** — GitHub Actions CI, systemd + Caddy deploy assets, a nightly backup
  timer, `/api/ops/status`, `adamarker -verify-blobs` (blob/DB ref-integrity check).
  See [`docs/OPERATIONS.md`](docs/OPERATIONS.md) for install, TLS, backup, and restore.

Not yet built: Phase 5 (multi-model agreement), Phase 8 (reports beyond the override-rate
and cost-per-run subset shipped tonight — cross-exam comparisons stay open).

## Develop

Requires:

- **Go 1.26+** (per `go.mod`)
- **Docker** for dev/test Postgres, unless you provide Postgres yourself
- **Node 20+** with npm for the frontend build
- a C compiler for cgo packages

```sh
make dev            # Postgres (compose) + server on :8080 with dev login enabled
make test           # unit tests (no Postgres needed)
make test-integration  # spins up the test DB and runs everything
make frontend       # vite build -> embedded into the binary
make build          # -> bin/adamarker
make db-dump        # blobs tarball + pg_dump into backups/ (order per DECISIONS D15)
```

Dev login: `make dev` enables `POST /auth/dev-login` (development-only, double-gated);
set `ADAMARKER_BOOTSTRAP_ADMIN_EMAIL=you@ntu.edu.tw` once so the first login works, or
rely on `.env`. Production login can be email magic links via `ADAMARKER_EMAIL_PROVIDER`
plus `ADAMARKER_APP_BASE_URL`, with optional Google OAuth if configured. Vite dev server:
`cd frontend && npm run dev` (proxies to :8080).

Config is env-driven — see [`.env.adamarker.example`](.env.adamarker.example). **Vision
providers are managed on the app's Providers page** (base URL, models, rate limits, API
keys — keys stored encrypted under the auto-generated `data/secret.key`, DECISIONS
D11 v1/D16). Env keys (`DEEPSEEK_API_KEY`/`QWEN_API_KEY`) are only imported once onto an
empty database.

Live provider smoke test (costs a fraction of a cent, needs a key):

```sh
set -a; . ./.env; set +a
ADAMARKER_LIVE_IMAGE=path/to/answer.jpg go test -tags live -run TestLive -v ./internal/llm/anthropiccompat/
```

## Layout

```
cmd/adamarker/     main: config, migrations, HTTP server, River workers — one process
internal/
  config/          env-driven configuration (providers, oauth, dev-login gates)
  auth/            email-link/OAuth flow, sessions, allowlist policy, bootstrap
  httpapi/         routing, RBAC middleware, JSON handlers, blob streaming, export
  store/           pgx + sqlc queries; storetest harness; numeric (decimal-string) helpers
  domain/          first-draft seam sketches from Phase 0 (superseded by packages below)
  blobstore/       local-disk store: atomic writes, traversal-proof keys
  render/          PDFium (wazero WASM) renderer + deterministic fake
  ingest/          upload → roster match/quarantine → render → map → mask pipeline
  imaging/         redaction masking; MaskedImage type = the D10 privacy invariant
  grading/         methods, prompts/schema, snap/clamp, manual records, run planner/worker
  llm/             provider seam + anthropic-compat adapter + fake + registry
  publish/         publish state machine, snapshot service, email-send seam (Phase 6)
  email/           EmailProvider seam: file/smtp/postmark/none, regrade tokens, templates
  queue/           River wiring: control + per-provider queues, transactional enqueue
  roster/          roster CSV contract (D13)
  web/             go:embed of the built SPA
frontend/          React/TS/Vite/Tailwind SPA (embedded at build)
migrations/        goose SQL (embedded; run at startup)
docs/              spec, plan-gap analysis, DECISIONS.md, build plans
```
