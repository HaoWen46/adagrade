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
| Grading with no server and no database | [Offline fallback](#offline-fallback-adamarker-offline-grade) |
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

## Offline Fallback (`adamarker offline-grade`)

A standalone subcommand for the day the server is down, unreachable, or simply not wanted:
roster CSV + scanned PDFs in, a transcription bundle out, with **no database, no HTTP server
and no accounts**. It prints a warning banner before it does anything, then:
force-matches every page to a roster (student, problem) cell, masks the identity regions on
this machine, sends only the masked images to the configured LLM API for transcription, and
writes a LaTeX + Typst **source** bundle mirroring the web export (it does not compile
anything — you run `xelatex`/`typst` yourself).

It is a fallback, not a second product: there is no rubric, no grading, no review UI, no
publish and no email. Identification is *forced* — every page it can place gets placed,
including ones the server would have parked in the orphan queue for a human.

```sh
# demo fixtures shipped in this repo (10 students x 4 problems, 40 pages)
adamarker offline-grade \
  --roster data/demo/demo-roster.csv \
  --out ./offline-run --problems 4 \
  --id-regions ./id-regions.json \
  --base-url https://dashscope-intl.aliyuncs.com/apps/anthropic \
  --api-key-env QWEN_API_KEY --model qwen3-vl-plus \
  data/demo/demo-scan-pile.pdf
```

- Transcription is a **vision** call: `--model` must name a model that accepts images
  (`qwen3-vl-plus` is what this repo validates live against).
- The API key is never an argument — `--api-key-env` names the *variable* holding it.
  `--provider-kind` defaults to `anthropic-compat`; pass `openai-compat` for an OpenAI Chat
  Completions endpoint (OpenRouter, most gateways). `--provider NAME` is the alternative
  route: it looks the name up in the env provider table (`ADAMARKER_PROVIDERS` +
  `ADAMARKER_PROVIDER_<NAME>_{KIND,BASE_URL,API_KEY}`, or an auto-detected
  `DEEPSEEK_API_KEY` / `QWEN_API_KEY` / `OPENROUTER_API_KEY`) — there is no database here,
  so the app's Providers page is unreachable.
- **Stage the run.** `--stop-after match` does identification only — nothing is masked and
  nothing leaves the machine; read `match-report.csv`, then re-run with `--force`.
  `--stop-after mask` also writes the masked pages and `masked-preview.jpg`, so you can see
  exactly what *would* be sent before spending a token. Neither needs a provider.
- **Where identity lives.** `--id-regions FILE` (recommended) is the same
  `{"version":1,"regions":[{"kind":"student_id","x":…,"y":…,"w":…,"h":…}]}` geometry the
  app's mask-region editor produces; kinds are `student_id`, `name`, `problem_id`. Without
  it the fallback is `--id-band 0.18`: one full-width strip across the top of the page, read
  ONCE, with all three fields scored as substrings of that same strip — so an id candidate
  can score against text sitting in the name box, and masking covers the whole band
  including the problem number. It needs no configuration and it is looser on both counts;
  draw the regions if you can.

Prerequisites (the same local-OCR rung the server uses, D24):

- `make ocr-models` — downloads PP-OCRv5 server rec (~85 MB) + `ppocrv5_dict.txt` into `data/ocr/`.
- **onnxruntime >= 1.27** installed on the machine (`brew install onnxruntime`, or a distro package).
- Export all three: `ADAMARKER_OCR_MODEL`, `ADAMARKER_OCR_KEYS`, `ADAMARKER_ONNXRUNTIME`.
  Unlike the server, this mode *hard-fails* without them (exit 6): reading identity locally is
  what makes masking possible before anything is sent.
- Optional `ADAMARKER_REPORT_FONT` (`make report-fonts`) — embeds the CJK font path in the
  bundle's LaTeX preamble; without it the preamble falls back to the family name
  "Noto Sans TC", which must then be installed or every Chinese glyph is silently dropped.

Artifacts, all written under `--out` at 0600/0700 (every byte of it is student work):

```
run.log             one line per stage, with timings — page indices only, never identity
pages/pNNNN.jpg     every scanned page, as rendered
crops/              what the local OCR actually looked at (per page, per region)
match-report.csv    who each page was assigned to, and how confident   <- CHECK THIS
match-report.json   the same rows unrounded, plus the run's settings
unmatched/pNNNN.jpg the pages nobody could place (originals, for you to sort by hand)
masked/pNNNN.jpg    the only bytes that leave this machine
masked-preview.jpg  contact sheet of what the model saw where identity used to be  <- CHECK THIS
                    (60 tiles per sheet; overflow sheets are -02, -03, …)
bundle/{exam}-pN/   the professor's export: MANIFEST.csv, tex/, typ/, images/, _all.{tex,typ}
```

Before trusting a single line of output, open **`match-report.csv`** (sort by `status`:
`forced` rows are the ones the solver moved off the page's own best guess) and
**`masked-preview.jpg`** (if identity is still visible there, it was still visible to the API).
Unmatched rows carry a `reason`: `surplus` (more pages than roster×problem cells), `low-score`
(nothing on the page could be read), `ambiguous` (another student explains it nearly as well),
or `id-conflict` (the ID box legibly reads an ID that is not the assigned student's — three or
more edits away — so the assignment was vetoed however confident it looked).

| Exit | Meaning |
|---|---|
| 0 | success |
| 1 | unclassified failure — including a Ctrl-C, which leaves the artifacts written so far |
| 2 | bad arguments, or `--help` |
| 3 | roster missing, unreadable, or unparseable |
| 4 | scan file missing, unreadable, or undecodable |
| 5 | `--out` unusable (not a directory, unwritable, or non-empty without `--force`) |
| 6 | local OCR unavailable or failed to load |
| 7 | `--id-regions` file invalid |
| 8 | provider unconfigured, or every transcription call failed |
| 9 | zero pages matched — the reports are still written; read them |

Honest limits:

- **Wrong-student assignments are possible by design.** A forced matcher has no "I don't
  know" for a page it can score: scores are posteriors over *this roster*, so "not on it"
  is not a hypothesis the matcher can hold. One case is caught — a page whose ID box
  *legibly* reads a different ID (≥ 5 characters, ≥ 0.90 recognizer confidence, ≥ 3 edits
  from the assigned student) is vetoed as `id-conflict` — but an ID that is smudged,
  cropped, or misread into a near-miss is not, and neither is a page identified purely on
  its name. Review the `forced` rows, and don't hand out grades from a run nobody read the
  report of.
- The `--min-score` / `--min-margin` defaults (0.15 / 0.03) are tuned on the synthetic,
  *printed* demo fixtures in `data/demo/`, not on real handwriting. Real scans are worse.
  Raise them to set more aside; lower them to place more, and read every row.
- Matching leans on the student-ID box (weight 0.45, against name 0.30 and problem 0.25,
  never renormalized). When the id box is unreadable, a name and a problem number alone
  rarely clear the margin against a full class — those pages land in `unmatched/`. The
  name channel is also the least exercised end to end: the committed `data/demo/` PDFs
  reference a CJK font they do not embed, so their name boxes render blank under this
  renderer and the 0.30 name term is covered by unit tests only.
- Pages set aside are never sent to the API and never appear in the bundle; they are copied
  into `unmatched/` as the *original* (unmasked) page, because a human on this machine has
  to look at them to place them.

The end-to-end test over the demo piles lives in `internal/offline/integration_test.go` and is
skipped unless the three `ADAMARKER_OCR_*` variables are set.

## Develop

Agents (Claude Code etc.): follow [`AGENTS.md`](AGENTS.md) instead — same facts, condensed.

Requires:

- **Go 1.26+** (per `go.mod`)
- **Docker** (daemon running) for dev/test Postgres, unless you provide Postgres yourself
- **Node 20+** with npm for the frontend build
- a C compiler for cgo packages

Starting from a machine with none of that installed? [`docs/BOOTSTRAP.md`](docs/BOOTSTRAP.md) is the from-zero playbook (macOS / Ubuntu / WSL2, Docker and no-Docker paths, agent-runnable).

### Quick start

0. `make doctor` — preflight; tells you exactly what's missing and how to fix it. Re-run until it says ready.
1. `make frontend` — build the SPA once (skippable, but without it the server serves a placeholder page).
2. Set the first admin: put `ADAMARKER_BOOTSTRAP_ADMIN_EMAIL=you@ntu.edu.tw` in `.env` (created on first login; only needed on an empty database).
3. `make dev` — starts the compose Postgres (:5433), runs migrations automatically, serves http://localhost:8080 with dev login enabled.
4. Open http://localhost:8080 and log in with the bootstrap admin email (the login page posts to `POST /auth/dev-login`; 403 `not authorized` means the email isn't an allowlisted user).

Scripting the API (curl, agents): every non-GET request needs the `X-ADA-CSRF: 1` header (any value), including dev-login:

```bash
curl -X POST localhost:8080/auth/dev-login -H 'Content-Type: application/json' -H 'X-ADA-CSRF: 1' -d '{"email":"you@ntu.edu.tw"}' -c cookies.txt
```

Alternative entry point: `./scripts/dev-e2e.sh` (what `.claude/launch.json` runs) — same thing on **:8899**, rebuilds the binary first, defaults the bootstrap admin + regrade-webhook dev vars itself, and prints the exact login call at startup. No Docker? Point `ADAMARKER_DATABASE_URL` (in `.env`) at any reachable Postgres 16+ and use this script — its compose step is warning-only.

### Everything else

```sh
make test              # unit tests (no Postgres needed)
make vet               # go vet
make test-integration  # spins up the test DB (:5434) and runs everything
make frontend          # vite build -> embedded into the binary
make build             # -> bin/adamarker
make db-dump           # blobs tarball + pg_dump into backups/ (order per DECISIONS D15)
make demo-data         # regenerate committed data/demo/ fixtures (deterministic)
make demo-walkthrough  # seed a completed demo exam into a RUNNING :8899 server
```

Optional features are off until their env vars are set — local OCR (`make ocr-models`), result-PDF attachments (`make report-fonts`), Typst math rendering (`ADAMARKER_TYPST_BIN`); each make target prints the env vars to set.

Production login is email magic links via `ADAMARKER_EMAIL_PROVIDER` plus `ADAMARKER_APP_BASE_URL`, with optional Google OAuth if configured. Vite dev server for frontend iteration: `cd frontend && npm run dev` (proxies to :8080).

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
