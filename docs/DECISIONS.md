# DECISIONS.md — flagged v0 defaults

*Started 2026-07-02 (overnight build session). Each entry resolves an open item from
[`docs/PLAN_GAPS.md`](PLAN_GAPS.md) (IDs referenced) with an explicit, reviewable default.
**Every decision here is a proposal encoded in code — flag and change any of them.**
Status: `v0-default` = implemented as described, awaiting your confirmation.*

---

## D1 — Answer identity & multi-page answers (B-C6, B-H13, B-M3) — `v0-default`

- An **Answer** is keyed by the natural key `(assessment_id, student_id, problem_id)` —
  one row per pair, `UNIQUE`, created (pre-materialized) for **every rostered student ×
  problem** when ingestion for an assessment first runs (B-H8: a no-show student is an
  Answer with zero pages, state `no_submission`, visible in every list).
- Pages live in **`answer_pages`** (ordered `page_index`), each holding
  `submission_id + pdf_page_index + image_ref + masked_image_ref + image_sha256`.
  Multi-page answers are therefore first-class; masking and grading apply per page.
- **No PDF splitting.** The submission PDF is stored once; answer pages reference
  `(submission PDF, page index)`. Rendering reads pages directly from the source PDF.
  `pdfcpu` is used for validation/page-count only. (Resolves A3: the `Renderer` seam takes
  the whole PDF and renders selected pages.)
- **Re-upload:** a new submission for the same `(assessment, student)` marks the old one
  `superseded_by` and re-points the answer pages. If any answer of that student already has
  grading records, re-upload is **blocked** unless the caller passes `force=true`, which
  flags every affected answer `image_superseded` (records store the image SHA they graded —
  provenance survives). Published answers can never be silently re-pointed.

## D2 — Status is *derived*, never stored (B-H1, B-C3 partially) — `v0-default`

There is **no `status` enum column** on answers. The DB stores facts; every list/badge
derives status at query time from them, so drift is impossible:

| Fact | Column |
|---|---|
| has pages? | `answer_pages` rows exist |
| AI-graded? | `grading_records` with `source='model'` exist |
| human-graded? | `grading_records` with `source='human'` exist |
| official set? | `answers.official_record_id IS NOT NULL` |
| official source | joined `grading_records.source` |
| published? | `answers.published_at IS NOT NULL` (Phase 6 sets it) |
| flagged? | `answers.flags` (text[]: `image_superseded`, `low_confidence`, `illegible`, `mask_review`, manual) |

Derived lifecycle for display: `no_submission → ungraded → ai_graded / human_graded →
official_set → published`, with `flagged` as an orthogonal highlight. Problem-level and
assessment-level rollups are **count summaries** (n graded / n flagged / n missing), not a
single enum.

## D3 — Assessment total & aggregation (B-C2) — `v0-default`

- A student's **problem grade** = the `total` of the answer's official record; **NULL when
  no official grade** (never silently 0).
- A student's **assessment total** = Σ official totals, computed by view/query, shown as
  partial when any problem is NULL (`42 / 100 · 2 problems ungraded`). Nothing is
  materialized in v0; a publish gate (Phase 6) will require 100% official coverage before
  emailing (B-M1).
- Missing/blank ≠ 0 automatically: a TA records a 0-score human record for a blank answer
  (auditable, B-H8); `no_submission` stays NULL until a human decides.

## D4 — Points, granularity, total authority (B-C8, B-M10) — `v0-default`

- All points are **`NUMERIC(6,2)`** in Postgres, decimal-string in Go/JSON (never float64).
- Each rubric version carries **`score_increment`** (default **0.5**). Model & human scores
  are **snapped to the increment and clamped to `[0, criterion.points]` in Go**; any snap
  is recorded in the record's `adjustments` field.
- The **app computes `total = Σ clamped per-criterion scores`**; a model-supplied total is
  stored in `raw_output` but never trusted (B-C8.2).
- **Rubric-save invariant: Σ criterion points == problem.max_points**, enforced at rubric
  version creation (400 otherwise).

## D5 — Versioning & immutability (B-H18, B-H20, B-L1) — `v0-default`

- `rubric_versions`, `reference_solution_versions`, `prompt_template_versions`, and
  `grading_method_versions` are **insert-only**: editing creates version N+1.
- A **run snapshots** `method_version_id` at launch; the method version pins rubric version,
  prompt version, and reference-solution versions (B-L1: records are reproducible w.r.t.
  reference content too). Records store all of them plus the resolved model id and
  temperature (B-H2; **temperature default 0**).
- Answer view renders a record against its **version-as-graded**, not the current rubric.

## D6 — Official grade precedence & concurrency (B-H3, B-H16, B-M2) — `v0-default`

- Setting `official_record_id` is a single guarded SQL statement with:
  - **optimistic concurrency**: caller sends `expected_official_record_id`; mismatch → 409;
  - **human lock**: if the current official record has `source='human'`, only an explicit
    human action (never a run/bulk path) may move the pointer.
- Run planners **include** human-finalized answers (grade for comparison) but **never touch
  the pointer**; Phase 5 Reconcile will obey the same lock.
- All pointer moves are audit-logged.

## D7 — OAuth hardening & session policy (B-H6, A1) — `v0-default`

- Full flow: random `state` (pre-session cookie) + **PKCE (S256)** + ID-token **`nonce`**
  + `aud`/`iss`/expiry verification + **session token rotation on login**.
- A1 sub-rules stand as committed in Phase 0: allowlist authoritative; personal-Gmail
  allowlisted → allowed; mismatched `hd` → denied; normalization = lowercase+trim.
- Sessions: scs + pgxstore, 7-day lifetime, `Secure/HttpOnly/SameSite=Lax`. Mutating API
  routes additionally require the `X-ADA-CSRF: 1` custom header (forces a CORS preflight
  cross-origin, which same-origin SPA fetch supplies trivially).

## D8 — Admin bootstrap & dev login (B-L2) — `v0-default`

- `ADAMARKER_BOOTSTRAP_ADMIN_EMAIL`: at startup, if set and no active admin exists, that
  email is upserted as an active admin (fixes first-deploy lockout).
- **Dev-only login bypass**: `POST /auth/dev-login {email}` exists **only when
  `ADAMARKER_ENV=development` AND `ADAMARKER_DEV_LOGIN=1`**; it still consults the
  allowlist. Never compiled out, but double-gated — needed to build/test the UI without
  Google credentials.

## D9 — Frontend build & embed wiring (A8) — `v0-default`

- Vite builds `frontend/` → `internal/web/assets/dist/` (gitignored). `web.Handler` serves
  `assets/dist` when present in the embed, else the committed placeholder `assets/index.html`.
  `make frontend` runs the Vite build; `make build` depends on it.
- Dev loop: Vite dev server on `:5173` proxies `/api` + `/auth` to Go on `:8080`.

## D10 — Blob delivery & the masked-only invariant (A4, B-M4) — `v0-default`

- **All images/PDFs stream through authenticated handlers** (`/api/...`); no signed URLs.
- The grading pipeline consumes a **`domain.MaskedImage`** value type constructible only by
  the `imaging` package (unexported field), and the provider request builder accepts only
  that type — "unmasked to provider" becomes a compile error, not a convention. A NULL
  `masked_image_ref` **fails the leaf** with `mask_missing`; there is no fallback to the
  original.
- v0 masking QA (toward B-C4/B-H17): a **keyboard-paced masked-crop review screen** listing
  every masked page for an assessment (accept / flag per page); grading runs refuse to
  launch while any in-scope page is unreviewed or flagged `mask_review`. Full OCR-based
  detection stays deferred.

## D11 — Providers & models — `v1` *(revised 2026-07-02 per user feedback)*

- **Providers are app-managed data** (`llm_providers` table): name, kind
  (`anthropic-compat` for now), base URL, suggested models, per-provider rate limit
  (rps + burst), enabled flag — all editable on the **Providers page**, no `.env`
  editing. API keys are entered in the UI, stored **AES-256-GCM encrypted** (see D16),
  and only a `…tail` hint is ever displayed again. A live **Test** button verifies
  key + URL (and pulls the model list where the endpoint supports it).
- Two wire adapters, selected per provider row by `kind`: **`anthropic-compat`**
  (Anthropic, DeepSeek/Qwen compatibility endpoints) and **`openai-compat`**
  (**OpenRouter** — many vendors' models behind one key —, OpenAI, and other
  Chat-Completions gateways). Presets in the UI carry base URLs + where-to-get-a-key
  guides; custom endpoints pick the API style explicitly.
- **Env keys** (`OPENROUTER_API_KEY`/`DEEPSEEK_API_KEY`/`QWEN_API_KEY`/
  `ADAMARKER_PROVIDERS…`) are import seeds: on boot, any env-detected provider whose
  **name is missing** from the table is inserted (encrypted) with sensible defaults —
  "paste key in .env, restart" works. Existing rows are never touched by env; the app
  UI owns them.
- The grading runner resolves providers through a cached DB-backed source; grading
  leaves share a single `llm` River queue (per-provider queues died with static config),
  throttled by each provider's own rate limiter.
- A deterministic **`fake` provider** still backs tests (via a static source).
- **Batch APIs remain deferred** (sync-only execution; `execution_mode` stays in the
  schema so batch is additive; B-M21's threshold decision deferred with it).

## D12 — Run leaves, retry, terminal states (B-H4, B-M7, B-M8) — `v0-default`

- **`grading_run_items`** (one per `(run, answer, model)`) tracks
  `pending/running/succeeded/failed/skipped`, attempts, error, resulting record id.
- A run is `completed` only when every item is terminal; items failing past
  **max 3 attempts** (or past the re-ask cap with malformed output) become `failed` with
  the error preserved — **no record is written, never a silent 0** (B-M8: unable-to-grade
  routes to humans via the `failed` state + flag).
- Model refusal path ("illegible/uncertain") **does** write a record with
  `confidence='illegible'`, score NULLs → flags the answer, counts as succeeded.
- Operators get **`POST /api/runs/{id}/retry-failed`** (re-enqueues only failed items).
- Progress = `GROUP BY state` over items (restart-safe); v0 UI polls, SSE later.

## D13 — Roster CSV contract (B-H9) — `v0-default`

- UTF-8 CSV, required header `student_id,name,email` (BOM tolerated, extra columns
  ignored). `student_id` is the identity & upsert key on re-import. Duplicate `student_id`
  in-file → whole import rejected with row numbers. Import returns an
  added/updated/unchanged report. Filename convention for submissions: `<student_id>.pdf`;
  non-roster filenames are **quarantined** into the mapping-review list, never dropped.

## D14 — Logging & PII policy (B-H12, CLAUDE.md) — `v0-default`

- Structured `slog` with an explicit allow-list convention: **IDs only** — never student
  names/emails, answer content, transcriptions, raw model output, or tokens. River job args
  carry row IDs only. Enforced by review + a `logsafe` helper package for common values.

## D15 — Migration & backup posture (B-L3, B-C1 partial) — `v0-default`

- goose migrations embedded, run in-process at startup, **each with a down**; River's own
  migrations run via its migrator. `make db-dump` produces a timestamped `pg_dump` next to
  a blob-dir tarball, ordered blobs-then-DB per B-C1 (full backup automation stays open).
  **Back up `data/secret.key` alongside** (see D16).

## D16 — Credentials master key (with D11 v1) — `v0-default`

- Stored credentials are sealed with **AES-256-GCM** under a 32-byte master key the app
  **generates on first boot** at `ADAMARKER_SECRET_KEY_FILE` (default
  `./data/secret.key`, 0600). No secret env vars needed for day-to-day use.
- Trade-off, explicitly: anyone with **both** the DB and that file can recover the API
  keys; the file must be included in backups (losing it just means re-entering keys in
  the UI — the app refuses to silently regenerate over a corrupt key file).
- Rotation = re-enter keys after replacing the file (documented; automated re-wrap
  deferred with B-H11).

## D17 — Multi-model consensus is post-hoc aggregation, not a run mode (B-C7; user-designed 2026-07-02)

Replaces spec §6.4's "multi-model method + Reconcile step". **Methods stay
single-model.** Consensus is a separate, re-runnable decision step over records that
already exist — **zero new API calls**.

- **Per-assessment policy** (`aggregation_policies`, one row per assessment — one
  standard for every problem/submission in that assessment; the next exam may differ):
  a **panel** of method versions, a **combiner** (`majority` | `mean`), a **fault
  tolerance** `f` (models allowed to be missing/outlying; validated `2f < n` — "cannot
  exceed half"), **flag triggers** (subset of `agg_disagreement`, `agg_missing`,
  `agg_low_confidence`), and `set_official` (auto-point unflagged answers).
- **Usable record** for (answer, panel method): the latest model record from that
  method version with non-NULL total and rubric_version equal to the problem's
  current latest (B-H20: never mix rubric versions).
- With `u` usable of `n` panel methods: `u < n − f` → `agg_missing`, nothing written.
  Otherwise combine **per criterion** (D4 increments make exact match meaningful):
  - `majority`: consensus value = any value shared by ≥ `n − f` records; no such
    value → criterion *contested* (recorded value falls back to the snapped mean so
    humans see a starting point).
  - `mean`: value = snapped mean; contested when more than `f` records sit further
    than one increment from it.
  Any contested criterion → `agg_disagreement`. More than `f` usable records with
  confidence low/illegible → `agg_low_confidence`.
- Result = an **append-only `grading_records` row with `source='aggregate'`**
  (total app-computed; raw_output carries the policy snapshot + input record ids +
  contested criteria; created_by = the TA who ran it). Consumers use the latest
  aggregate per answer; re-running appends (derived artifacts, honest history).
- Aggregation owns exactly the three `agg_*` answer flags — it adds *and clears*
  them on each re-run. Official pointers move only through the guarded path (D6):
  never over a human decision, never on flagged answers, never once published.
- `n=1, f=0` degenerates to "bulk accept from one method" — same machinery.

## D18 — Scan intake is a staging pipeline; promotion reuses the ingest tail — `v0-default`

*(Spec: [`2026-07-02-scan-intake-identification-design.md`](superpowers/specs/2026-07-02-scan-intake-identification-design.md); resolves spec §13 "exact submission format", B-H17 partially.)*

- Randomly-named exam scans (zip / loose PDFs / single-page images) land in
  **staging tables** (`scan_batches`, `scan_files`) that never touch the graded
  domain. Async River jobs (`scan.expand`, `scan.render`, `scan.identify`) render
  page 0, crop the ID region, OCR it, and propose a roster match.
- **Every file requires human confirmation** (keyboard-paced review of the ID crop);
  a proposal is never terminal. Two files can't be confirmed to one student.
- **Finalize** gates promotion: all files terminal (assigned/discarded), missing
  active students explicitly acknowledged (audit-logged). Promotion calls the same
  ingest tail as `AssignQuarantine` — every D1 guard applies unchanged. Idempotent,
  re-runnable per file.
- Post-finalize corrections: `reassign` retracts the wrong student's submission
  (new `submissions.retracted_at` tombstone; page deletion scoped to the retracted
  submission) and re-promotes — same graded/published guards.

## D19 — Provider images: identity ⊕ answer content (D10 carve-out) — `v0-default`

- The privacy invariant generalizes: **a provider request carries identity XOR
  answer content, never both.** Grading sends masked answers (no identity);
  identification sends a tight ID-box crop (no answer content).
- `llm.Request.Images` becomes `[]imaging.ProviderImage`, a **sealed interface**
  implemented only by `imaging.MaskedImage` and `imaging.IDCrop` (constructible only
  via `imaging.Crop` / `imaging.LoadIDCrop`, key-gated on `/idcrop/`). Arbitrary
  unmasked pages remain a compile error.
- Sending the ID crop (student ID + name) to a third-party VLM is a **deliberate,
  documented PII carve-out** — identifying 200 papers is the point. Opt-out:
  `scan_batches.ocr_enabled=false` runs the identical flow with zero provider calls.
- OCR text lives in `scan_files` columns (DB already holds the roster) but never in
  logs/job args/error strings (D14).

## D20 — ID regions are their own table, cropped not painted — `v0-default`

- `id_regions` mirrors `mask_regions` (per-assessment normalized rects,
  PUT-replaces-all) with `page_index` instead of `page_scope`. Separate table
  because `ApplyMasks` paints *every* mask region — co-mingling would black out the
  box identification needs to read. UI offers copy-to-mask (the ID box is usually
  exactly what grading should mask).
- New `imaging.Crop` reuses `Mask`'s floor/ceil+Intersect pixel math incl. padding;
  multiple rects stack vertically into one crop JPEG.

## D21 — OCR→roster matching ladder — `v0-default`

- Normalization: IDs — NFKC (full-width→ASCII), uppercase, strip non-alphanumerics;
  names — NFKC, strip all whitespace, case-fold Latin (CJK exact).
- Ladder (first hit proposes; filename convention still honored): `filename` →
  `ocr_id` (exact) → `ocr_fuzzy` (unique Levenshtein≤1 + name confirms) →
  `ocr_name` (unique exact) → unidentified. Filename-vs-OCR disagreement flags a
  conflict. Withdrawn students are never candidates.
- Matching is pure Go, table-tested; model output is never trusted into an
  assignment (same philosophy as D4's snap/clamp).

## D22 — Image files & per-problem submissions — `v0-default`

- `submissions` widens: `pdf_ref/pdf_sha256` → `source_ref/source_sha256`, new
  `source_kind ('pdf','image')`, new nullable `problem_id`. Images (png/jpg) are
  normalized at intake (decode → downscale → JPEG q85) into a synthetic 1-page
  render; the original bytes stay the stored source.
- A batch with `problem_id` set means one file = one submission **for that problem**
  (exam-scanned-per-question / image-per-question). Active-uniqueness becomes two
  partial indexes (whole-assessment vs per-problem scope); supersede/force/published
  guards evaluate within the scope. Page deletion on supersede/retract is scoped to
  the affected submission (was: all pages for the student).

## D23 — Withdrawn students — `v0-default`

- `students.withdrawn_at` (nullable), toggled via `PATCH /api/students/{id}`
  (lecturer+, audit-logged). CSV re-import never touches it.
- Withdrawn students are excluded from `MaterializeAnswers`, missing-lists / ingest
  report expectations, matching candidates, and (Phase 6) publish. Existing
  answers/records remain untouched.

## D24 — Local OCR as the first identification rung — `v0-default` *(user-requested 2026-07-02: "very small local model, just in a program")*

- **Engine**: PP-OCRv4 mobile *recognition-only* ONNX (~11 MB, one model covers
  digits + Latin + Chinese) via onnxruntime (`yalue/onnxruntime_go`, dlopen —
  requires **onnxruntime ≥ 1.27** / C API v26, verified empirically). No detection
  model: the crop is a tight TA-drawn box, so lines are split by pure-Go horizontal
  ink-projection. Live E2E test (opt-in, env-gated) decoded a rendered student ID
  at 0.98 confidence.
- **Seam**: `ocr.Reader` (internal/ocr) consumes `imaging.IDCrop` — the same sealed
  D19 artifact as the provider path, so local OCR can't be pointed at answer
  content either. Implementation in internal/localocr; `scan.Service.Local` nil ⇒
  feature off, everything behaves as before.
- **Ladder becomes**: filename → **local OCR** → cloud VLM → human. Local runs
  whenever a crop exists and the engine is configured — `batch.ocr_enabled` now
  gates only the *cloud* call (the PII-leaving-the-machine part, D19); with local
  configured and OCR disabled, identification is fully offline. A local hit skips
  the VLM entirely. `scan_files.ocr_engine` records which engine produced the
  ocr_* fields ('local' or the provider name).
- **Guards**: lines below mean confidence 0.60 are ignored; ID/name are extracted
  from mixed lines as the longest ASCII-alnum run (≥5) / Han run (≥2) *before*
  matching (NormalizeID keeps CJK, so raw mixed lines would never exact-match);
  local errors log IDs-only and fall through — local OCR can never wedge a file.
  Human confirmation stays mandatory (D18) regardless of engine.
- **Ops**: models + libonnxruntime are runtime files configured by env
  (`ADAMARKER_OCR_MODEL`, `ADAMARKER_OCR_KEYS`, `ADAMARKER_ONNXRUNTIME`), fetched
  by `make ocr-models`, never committed. Missing/invalid config ⇒ startup warning,
  feature disabled, no hard failure.
- Known limits (documented in code): BGR channel order kept on Paddle convention
  (grayscale scans make it moot); handwritten-CJK name quality is best-effort —
  the ID is the load-bearing signal and the name only confirms the fuzzy rung.
- **Amended 2026-08-06 — the engine is now PP-OCRv5 server rec.** The ~11 MB v4
  mobile export above is superseded by the PP-OCRv5 *server* recognizer (84.5 MB,
  `PP-OCRv5_server_rec_infer.onnx` + `ppocrv5_dict.txt`, 18,383 classes covering
  Traditional Chinese and materially better on handwriting), fetched by the same
  `make ocr-models`. Two mechanical consequences: the export emits RAW LOGITS
  rather than ending in a Softmax node (detected per-row, so either shape still
  decodes), and lines are recognized at `recMaxW=1280` instead of the v4-era 640.
  The rationale is the `offline-grade` work, whose closed-set lattice scoring
  needs a charset that can actually spell the roster. **v4 assets remain
  loadable** — the class count is still validated against the keys file rather
  than hard-coded (`validateClassCount`), so an operator who has not re-run
  `make ocr-models` gets the older charset, not a startup failure.

## D25 — Grading policies: curated stances, prompts as firmware — `v0-default` *(user-designed 2026-07-02)*

- **Philosophy.** "Approaches are data, not code" never meant TAs write prose to
  LLMs. The TA-facing knob is a **policy** — a named grading stance; the prompt
  text implementing it is **firmware**: system-curated, versioned, previewable,
  never editable (this formalizes the existing read-only template status quo).
  What TAs choose must be semantically meaningful to an educator.
- **Policy changes judgment under ambiguity, never the rubric.** The rubric fixes
  what earns points, identically for every policy; the policy fixes only what the
  rubric leaves open. Three levels, each answering the same four questions
  (evidence threshold / ambiguity direction / error cascade / when to flag):
  - **lenient** — benefit of the doubt: reward the visible idea, ambiguity →
    student's favor, follow-through after slips, flag only the unreadable.
  - **standard** — rubric-faithful (default; the pre-D25 prompt's stance):
    exactly what the rubric supports, plausible-human-reading tie-breaks.
  - **strict** — exam standard: only complete demonstrated work, ambiguity →
    lower score, no follow-through unless the rubric grants it, **prefers
    flagging over guessing** (raises the human-review rate by design).
- **Mechanics.** `MethodConfig.policy` (default standard); ONE template
  (`transcribe-then-grade` v2) containing all three branches (`{{if eq .Policy}}`)
  — stance sentence in the system prompt + a `# Judgment policy` section before
  the instructions; rendered via `PromptData.Policy` at the single BuildPromptData
  choke point, so the runner and the prompt-preview stay byte-identical. The
  seeder now appends a new template version whenever the Go constants change
  (old pins untouched). Reproducibility: records pin the template version (all
  branch text in DB) **and** a new `grading_records.policy` column (migration
  0012; NULL = human/aggregate/legacy).
- **Honesty surfaces.** A 7/10 under strict ≠ 7/10 under lenient: policy badges on
  record history (with method/prompt version provenance now exposed), a policy
  column in Analysis, a warning when a consensus panel mixes policies, and a
  **fairness check** — Analysis lists any problem whose official grades mix
  policies (one standard per problem; becomes a publish-gate input in Phase 6).
  Guard: non-standard policy on a pre-policy template version is rejected (it
  would silently no-op). Preview accepts a `policy` override so all three
  stances can be read before saving a method.
- **Not built, on purpose:** free-text prompt editing, per-run policy overrides,
  custom/numeric strictness. A fourth stance, if ever needed, gets added here —
  curated, shared, versioned.

## D26 — Single worker fleet per database (advisory lock) — `v0-default` *(incident-driven 2026-07-02)*

- **Incident**: two stale `./bin/adamarker` zombies ran River workers against the
  dev DB for ~19 h and *graded new jobs with old code* (NULL-policy records; a
  leaf failed with `unknown kind "openai-compat"`). Nothing prevented multiple —
  possibly stale — worker fleets from sharing one queue.
- **Rule**: before starting River workers the process takes
  `pg_try_advisory_lock(hashtext('adamarker:workers'))` on a **dedicated
  connection** (session locks die with pooled conns) held for process lifetime.
  Lock unavailable ⇒ **fatal** with a clear message. Escape hatch
  `ADAMARKER_ALLOW_MULTIPLE_WORKERS=1` downgrades to a loud warning for
  deliberate experiments. See `internal/queue/workerlock.go`.
- Full audit context: [`docs/audits/2026-07-02-stability-efficiency.md`](audits/2026-07-02-stability-efficiency.md)
  (24 confirmed findings; 10 fixed same-day, 5 spun off as follow-up tasks).

## D27 — Batch operations are queue jobs, not request work — `v0-default` *(audit F1/F2/F16, 2026-07-03)*

- **Rule**: any operation whose work scales with roster×pages (finalize
  promotion, mask application, bulk direct upload) validates its gates in the
  request, enqueues per-item River jobs, and returns **202**; the UI polls the
  existing derived-state counters. Request-scoped work at 200×9 scale could
  never outlive the browser's fetch abort (audit F1/F2).
- **Jobs** (queue `scan`, MaxAttempts 3): `scan.promote` (per assigned file;
  skips promoted; the last completing job sets `finalized_at` via a
  guarded idempotent update), `mask.page` (per page), `ingest.direct` (per
  staged upload; results persisted on the new `direct_uploads` row — statuses
  mirror ingest.FileResult; `GET /api/assessments/{id}/uploads` serves the
  polling UI). Business rejections are terminal per-item results, never
  retries; finalize's 409 gate contract is unchanged.
- **Mask fingerprint** (migration 0014, `answer_pages.mask_input_sha`): the
  masked artifact records a hash of its inputs (original image SHA + quality +
  effective region set), so re-applying skips up-to-date pages **and preserves
  their review status** — previously every re-apply redid all pages' CPU work
  and reset accepted reviews.
- Verified end-to-end against live River workers (upload→ingested/quarantined;
  apply→masked→re-apply all-skipped), not just unit suites.

## D28 — Publish model: per-assessment, snapshot-based, append-only (B-C3, B-M1) — `v0-default`

- Publishing operates on a whole **assessment**, not per-answer/per-problem. `publish_batches`
  (one row per publish action) + `publish_items` (one row per included student: JSONB
  snapshot of per-problem totals/per-criterion scores/comments + assessment total,
  recipient email captured **at publish time**, regrade token, `email_status`).
- **Coverage gate:** publishable only when every (roster student × problem) answer either
  has an official grade or is `no_submission` (B-M1's completeness precondition). The
  preview enumerates blockers instead of failing opaquely.
- Publishing sets `answers.published_at` for every answer in the assessment and enqueues
  one send job per item; students whose every answer is `no_submission` get an
  `email_status=skipped` item — no email sent.
- **Lock:** official-grade changes on a published answer 409 while `published_at` is set,
  turning the pre-existing read-only guard into an enforced lock.
- **Single-live-batch invariant** (post-spec hardening, same day): publish now checks for
  an existing non-superseded batch up front and 409s (`ErrAlreadyPublished`) rather than
  creating a second live batch — the first implementation let a double-click or retried
  request produce two live batches, duplicate emails, and an ambiguous
  `LatestNonSupersededBatch` for unpublish. Re-publish is always unpublish → publish.
- **`not_ingested` fail-closed blocker** (post-spec hardening, same day): coverage queries
  originally started `FROM answers`, so a roster student added *after* ingest (zero
  answers rows — distinct from a genuine `no_submission` answer row) silently passed the
  gate and received no email at all. Coverage now also walks students `LEFT JOIN answers`
  and counts such a student as a distinct `not_ingested` blocker, failing closed.

## D29 — Unpublish is an admin-only escape hatch, not an undo (B-C3) — `v0-default`

- `POST /api/assessments/{id}/unpublish` (admin-only, audit-logged) clears `published_at`
  on the batch's answers and stamps the batch `superseded_at`. It **does not un-send
  email** — it re-opens grading so a correction can be made and re-published.

## D30 — Re-publish defaults to changed-only (B-C3) — `v0-default`

- Re-publishing creates a new batch. Default selection is **changed-only**: items whose
  snapshot differs from the same student's item in the latest prior batch, with an
  explicit "resend to everyone" (`resend_all`) toggle. The email template for a
  re-publish says "corrected results" rather than the first-send wording.

## D31 — Email provider seam: four implementations behind one interface (B-H10 partial) — `v0-default`

- `internal/email` implements `domain.EmailProvider` as `file` (writes `.eml` under
  `<blobdir>/../outbox/`; default in development), `smtp` (stdlib `net/smtp` +
  `crypto/tls`, STARTTLS on 587 or implicit TLS on 465), `postmark` (HTTP API via stdlib
  `net/http`; the plan's intended production provider; `ParseInbound` decodes Postmark's
  inbound JSON), and `none` (records everything, marks items `skipped`, loud warnings).
  Selected by `ADAMARKER_EMAIL_PROVIDER`.
- Production with a real provider requires `ADAMARKER_EMAIL_FROM` set or config load
  fails loudly (mirrors the OAuth config rules). `ADAMARKER_EMAIL_REPLY_DOMAIN` unset ⇒
  Reply-To omitted and the email says replies aren't monitored.
- **PII rule:** message bodies are never logged; logs carry only counts, statuses, and
  item ids (CLAUDE.md). Send pipeline is one River job per `publish_item` on a dedicated
  `email` queue, rate-limited (default 1/s), with F17 drain semantics (a
  drain-cancelled send stays `pending`, never `failed`).

## D32 — Regrade token: HKDF off the existing master key, not single-use (B-H10, B-H11) — `v0-default`

- Format `v1.<publish_item_id>.<expiry-unix>.<base64url(HMAC-SHA256(...))>`. The HMAC key
  is a subkey derived from the existing D16 machine-local master key via
  `HKDF(info="regrade-token-v1")` — no new secret to provision or back up.
  `ADAMARKER_REGRADE_WINDOW` (default 14 days) sets expiry.
- The token identifies a `publish_item` (⇒ student + assessment + snapshot), **not
  single-use** — repeats are governed by the D33 rate cap, not token consumption.
  Verification is by recomputation, not DB lookup, so the webhook path can't be used to
  enumerate items.

## D33 — Inbound verification is a 5-rung ladder, SPF/DKIM warn-not-block (B-H10) — `v0-default`

- `POST /webhooks/email/inbound/{secret}` — path secret from
  `ADAMARKER_INBOUND_WEBHOOK_SECRET`, constant-time compared, 404 on mismatch (no oracle).
  Ladder, every rejection recorded with a reason, **no reply sent for unverified mail**
  (no backscatter): (1) token parses + HMAC valid + not expired; (2) its batch is not
  superseded; (3) sender email equals the student's **current** roster email
  (case-insensitive — a roster-email change invalidates old addresses, resolving B-H10's
  stale-email question); (4) SPF/DKIM verdicts recorded but **v0 warn-not-block**
  (flagged for review — university mail forwarders routinely break strict SPF/DKIM);
  (5) rate cap of `ADAMARKER_REGRADE_MAX` (default 3) *verified* requests per (student,
  assessment) — beyond that, status `rejected/rate_limited`, still visible in the queue.
- **MessageID idempotency** (post-spec hardening, same day): Postmark retries inbound
  webhook deliveries on timeout/non-2xx; without a dedup key a redelivered payload created
  a duplicate verified regrade row, burned rate-cap budget, and double-sent the
  confirmation email. Migration 0020 adds `regrade_requests.message_id` (nullable — not
  every caller/fixture supplies one) with a partial unique index (`WHERE message_id <>
  ''`) so redelivered messages collide and no-op instead of duplicating.

## D34 — Regrade may lower a grade; UI warns, does not block (B-H15) — `v0-default`

- The regrade-may-lower-grade policy question (B-H15) is resolved as **warn-not-block**:
  resolving a regrade to a lower score is allowed; the Regrades UI surfaces it as a
  warning at resolve time, not a hard stop. No ratchet/no-detriment rule is enforced in
  code. Flagged for review — many institutions forbid appeals from lowering a grade.
  Resolving sends a resolution email; a changed grade puts the assessment into
  needing-re-publish, which changed-only re-publish (D30) picks up automatically.

## D35 — Model pricing is operator-entered data, cost computed at insert time (B-H5) — `v0-default`

- New `model_pricing` table (`provider_id`, `model`, `input_usd_per_mtok`,
  `output_usd_per_mtok`, unique on the pair), edited on the existing Providers page — not
  seeded from `docs/MODELS.md`, which stays a human cheat-sheet.
  `grading_records.cost_usd` is computed at record-insert time from token counts × the
  matching pricing row; **no pricing row ⇒ NULL cost_usd**, never a fake zero. No
  historical backfill — a pricing edit affects only future records (flagged).

## D36 — Two independent, fail-open-when-unconfigured budget caps (B-H5) — `v0-default`

- **Per-run cap:** nullable `grading_runs.cost_cap_usd`, set at run creation. The leaf
  executor checks the run's accumulated `SUM(cost_usd)` before each grade call; at/over
  cap, remaining leaves record a terminal `budget_exceeded` failure (retryable after
  raising the cap).
- **Monthly global cap:** `ADAMARKER_MONTHLY_BUDGET_USD`. Run creation compares
  month-to-date `SUM(cost_usd)` plus the new run's **pre-flight estimate** against it and
  refuses with a 409 (numbers included) when exceeded; raising the env var is the
  deliberate escape hatch.
- **Pre-flight estimate:** `answers × (1500 input + 400 output tokens)` × pricing, summed
  per model in the method (mirrors the heuristic in `docs/MODELS.md`), shown at run
  creation alongside month-to-date spend; says so explicitly when pricing is missing
  rather than printing a fake $0.
- Both caps are **fail-closed only when configured** — an unconfigured system behaves
  exactly as before tonight.

## D37 — Spot-check gate blocks bulk accept-official, not per-answer acceptance (B-C5) — `v0-default`

- A run's grades cannot be bulk-accepted as official (Runs "accept-official" action *or*
  an aggregation policy's auto-set-official path) until a human has spot-checked a sample
  of that run's records. Per-answer manual acceptance in AnswerView stays ungated — it
  already is human review.
- **Sample:** deterministic PRNG seeded by run id, stratified across the run's problems:
  `min(max(5, 5% of graded leaves), 20)` records. New `spot_checks` table
  (run_id, grading_record_id, verdict `agree|adjusted`, note, checker, timestamps). When
  every sample has a verdict, the gate opens; the accept-official confirm dialog shows the
  sample's agreement rate.
- **Override:** admin-only `POST /api/runs/{id}/spot-check/waive` with reason,
  audit-logged (`run.spotcheck.waive`). Runs that were already `completed` before this
  feature shipped are backfilled into `waived_runs` with `reason='migration'` (migration
  0019) so history isn't retroactively locked.
- **Canonical-first-sample fix** (post-spec hardening, same day): sample creation
  originally ran unconditionally on every pending→completed transition. A retry-failed
  cycle brings previously-failed leaves into the succeeded pool and re-completes the run,
  re-invoking sample creation against a larger pool; since `InsertSpotChecks` is only
  row-idempotent (`ON CONFLICT DO NOTHING`), the larger re-drawn sample **appended**
  unchecked rows atop ones a human had already verdicted, re-blocking a gate that was
  already clear. Fixed: sample creation now checks `SpotCheckState` first and no-ops if a
  sample already exists or the run is waived — the first sample drawn for a run is
  canonical, never re-drawn.

## D38 — Score distributions fall back to AI grades when officials are sparse, labeled (B-H14) — `v0-default`

- `GET /api/problems/{id}/score-distribution`: per-criterion and total mean, stddev,
  %zero, %max, and a 10-bucket histogram over **official** grades; falls back to the
  latest run's **AI** grades when officials are sparse, explicitly labeled as such in the
  response (never silently mixed). Surfaced in ReviewTab (replacing bare counts) and in
  the publish preview, so a systematic misread ("Problem 2 is all zeros") is visible to
  the operator right before they hit send.

## D39 — Audit log gets a read path; write side was already there — `v0-default`

- `GET /api/audit?target_kind=&target_id=&action=&actor=&limit=&offset=` (admin-only)
  reads the pre-existing 41+ action-type audit log; `detail` JSONB is included in the
  response but the UI renders it collapsed by default. Surfaced as an "Audit" section on
  the Users page (already the admin corner), newest-first, filterable, 50/page.

## D40 — Reports: override rate + cost per run, cross-exam stays deferred (Phase 8 subset) — `v0-default`

- **Override rate per method:** share of answers where the official grade's source is a
  human record that replaced or adjusted an AI record from that method's runs, plus mean
  `|Δ|` between the AI total and the final official total.
- **Cost per run:** run list gains `SUM(cost_usd)` + token counts; AnalysisTab shows cost
  per method per assessment and cost-per-answer.
- Cross-exam comparisons (comparing the same method/rubric across multiple assessments)
  stay deferred — recorded as still-open in PLAN_GAPS, not attempted tonight.

## Addendum — whole-branch review fixes (2026-07-03, N2 wave)

Seam defects surfaced by the whole-branch review, resolved without changing the
above decisions' intent:

- **D30 baseline is per-student-across-batches, not "the newest batch".** The
  changed-only diff baseline is now each student's most recent item across ALL of the
  assessment's batches (`DISTINCT ON (student_id) … ORDER BY student_id, batch_id DESC`).
  A changed-only re-publish writes a *thin* batch of only the changed students; keying
  the baseline off the newest batch alone made every absent student look changed and
  re-emailed the whole cohort on the next cycle (C1).
- **Resolution-email "New total" sourcing (D34).** A "regraded" resolution now sources
  the New total in priority order: (1) the student's item in the live non-superseded
  batch; (2) computed from current official grades; (3) genuinely omitted (the template
  gates the line on a non-empty total). The superseded token snapshot is never presented
  as the new total (C2).
- **D29+D30+D33 token re-bind.** At ladder rung 2, a token whose batch was superseded is
  RE-BOUND to the same student's item in the assessment's live batch (same
  assessment_id + student_id) and the ladder continues against it; sender verification
  and the rate cap run unchanged; with no live item it still rejects as
  `rejected_superseded`. No new oracle — the HTTP response is 200 either way. This
  replaces the old behavior where an unpublish permanently killed every unchanged
  student's regrade token (C3).
- **Confirmation + resolution emails send synchronously in-handler, not via the
  rate-limited D31 email queue.** This is a known, deliberate drift from D31's "one
  River job per send" model: these are single, low-volume, human-triggered sends where
  in-handler delivery keeps the resolve/verify flow simple. Kept as-is, documented here.
- **`ErrNothingToPublish` → 409 on an empty changed-only republish (D30).** A
  changed-only re-publish that selects zero students refuses with 409 rather than
  writing an empty batch that would clobber the diff baseline.
- **Single-live-batch invariant is DB-enforced (D28/D29).** Migration 0021 adds a
  partial unique index (`publish_batches(assessment_id) WHERE superseded_at IS NULL`);
  a racing second publish loses cleanly with the same already-published 409 the
  pre-check produces.
- **Regrade queue pagination.** `GET /api/regrades` accepts `?limit` (cap 200) &
  `?offset` with `has_more`; the queue was previously capped at the newest 50 rows with
  no way to page past them.

## D41 — Publish dialog shows the From address — `v0-default` *(2026-07-03 morning round)*

- The already-built `ADAMARKER_EMAIL_FROM` sender is now displayed in the publish
  dialog so the operator sees exactly what students will see before sending — no new
  mechanism, a clarification/UI surface only.

## D42 — Report PDF: new `internal/report` seam on `go-pdf/fpdf` — `v0-default` *(2026-07-03 morning round)*

- New sanctioned seam (`ReportRenderer`), justified like Renderer/BlobStore — stdlib has
  no PDF writer and CJK comments need real font embedding. Layout: A4 landscape per
  answer page, left half the student's **original, unmasked** page image (masking exists
  only for LLM calls — students see their own work), right half a grading panel
  (problem label, per-criterion `name score/max` + comment, problem total); the PDF's
  first page carries a header (assessment name, student name+ID, assessment total).
  Multi-page answers run image pages sequentially; the panel renders on the problem's
  first page, later pages marked "(continued)".

## D43 — Report font is a single env knob; unset disables attachments, not publish — `v0-default` *(2026-07-03 morning round)*

- Noto Sans TC ships via `make report-fonts` into `data/fonts/` (downloaded like OCR
  models, never committed); `ADAMARKER_REPORT_FONT` points at it. Feature-gated like
  local OCR (D24): unset ⇒ attachment options disabled in the UI with a hint, but
  publish without attachments still works. Unlike D24's three-var LocalOCRConfigured,
  this is one variable with no partial state (`Config.ReportFontConfigured()`).

## D44 — Exactly three attachment quality options — `v0-default` *(2026-07-03 morning round)*

- `none` (default, today's text-only email), `compressed` (recommended: page images
  downscaled to long edge 1600px, JPEG q75), `original` (page images as stored, at
  ingest render DPI/limits). No other tiers.

## D45 — ZIP fallback swaps the merged PDF for per-problem JPEGs — `v0-default` *(2026-07-03 morning round)*

- A checkbox alongside the quality picker swaps the merged PDF attachment for a ZIP of
  per-problem JPEGs at the chosen quality, for mail-gateway or PDF-viewer trouble.
  Filenames: `problem-<n>-page-<m>.jpg` plus `grades.txt` (the text body's breakdown) —
  no PII beyond the student's own content. Attachments are built at send time from
  blobs, not stored on `publish_items` (blobs stay the source of truth; rebuild-on-resend
  is deterministic); `publish_batches.attachment`/`.zip` record the batch's choice so
  resends reuse it. Any item whose built attachment exceeds 15 MB gets a non-terminal
  per-item warning (send still proceeds — SMTP servers reject over-limit mail with a
  visible failed status of their own).

## D46 — Individual resend, any status, reuses the batch's attachment settings — `v0-default` *(2026-07-03 morning round)*

- `POST /api/publish/items/{id}/resend` (lecturer+, audited `publish.resend_item`)
  re-enqueues one item's send job regardless of its current status — covers "student
  says they never got it." Distinct from the pre-existing `ResendFailed`, which only
  re-arms a batch's `failed` items. UI: a per-row "Resend" button in the batch history
  table.

## D47 — Regrade email copy now spells out reply-to-regrade explicitly — `v0-default` *(2026-07-03 morning round)*

- The template's regrade instructions now say plainly: "To request a regrade, **reply
  directly to this email** (keep the subject line), describe which problem and why,
  before <deadline>." Only replies carry the token; the copy states that fact instead of
  leaving it implicit.

## D48 — Regrade turns escalate to manual review before they reject — `v0-default` *(2026-07-03 morning round)*

- Verified requests at turn ≤ `ADAMARKER_REGRADE_MAX` (default 3) are `received` as
  before. Turns `MAX+1`..`ADAMARKER_REGRADE_HARD_MAX` (default 10) become **`received` +
  `escalated=true`** — visible in the queue, AI-assist disabled, badge "manual review
  required" (the original Phase 7 intent). Only beyond `HARD_MAX` does the ladder fall
  back to `rejected/rate_limited` as a table-flood backstop. Migration 0023 adds
  `escalated BOOL` + `turn INT` to `regrade_requests`; `turn = 1 + prior verified count`
  for the (student, assessment) pair, backfilled for existing rows.
- `ADAMARKER_REGRADE_HARD_MAX` must be strictly greater than `ADAMARKER_REGRADE_MAX` —
  validated at config load; raising `REGRADE_MAX` above the built-in default hard max
  (10) with `HARD_MAX` left unset fails boot loudly rather than silently picking a
  degenerate value.

## D49 — Best-effort reply→problem matching, stored as a TA-editable guess — `v0-default` *(2026-07-03 morning round)*

- Parses a regrade reply's subject+body for a problem reference — `problem 3`,
  `q3`/`p3`, `#3`, `第3題`/`第三題`, `问题3` (CJK numerals 1–20 mapped via the `十`
  construction) — and validates the parsed number against the assessment's actual
  problem numbers before storing it. Stored as nullable `regrade_requests.problem_id`;
  the detail UI shows it as an editable chip (`PATCH /api/regrades/{id} {problem_id}`),
  and the deep-link goes to that problem's AnswerView when set. Wrong guesses cost one
  click to fix; no guess falls back to the existing answer-list links. Pure Go,
  table-tested (`internal/regrade/match.go`), first-match-by-byte-offset wins when a
  reply mentions more than one problem.

## D50 — Stricter AI re-grade assist: `regrade_v1` template, never auto-official — `v0-default` *(2026-07-03 morning round)*

- A dedicated versioned prompt template kind `regrade_v1` (seeded like grading
  templates, read-only firmware per D25), single stance = stricter: context is the
  pinned rubric version, reference solution, the original per-criterion scores +
  comments from the contested official record, and the student's request text; framing
  is "an independent stricter re-examination — change a score only on demonstrable
  grading error in either direction; skepticism toward unsupported claims; do not reward
  persistence." Output schema is the standard grading JSON.
- Execution is a River job on the existing `llm` queue (provider rate limits apply).
  Result is an append-only `grading_records` row, `source='regrade_ai'`, policy pinned
  `regrade_strict`, linked via `regrade_requests.ai_record_id` (migration 0023) — never
  auto-official. The TA compares old vs new in the regrade detail and walks the normal
  unpublish→official→re-publish path; a "needs re-publish" chip already exists for that.

## D51 — AI re-grade privacy: masked images, mechanically redacted request text — `v0-default` *(2026-07-03 morning round)*

- The identity XOR content law (D19) holds for the AI re-grade path too. Images sent are
  the existing **masked** copies (sealed `ProviderImage` path) — never the originals.
  The student's request text is redacted before prompt assembly: exact-match removal of
  that student's roster name, student ID, email, and the `regrade+…` token string.
  Redaction is mechanical (`internal/regrade/redact.go`) and logged as counts only, per
  the CLAUDE.md PII rule.

## D52 — AI re-grade buttons are TA-only, with a batch dry-run cost estimate — `v0-default` *(2026-07-03 morning round)*

- Per-request "AI re-grade" (eligible: status `received`/`under_review`,
  `escalated=false`, no AI record yet or an explicit re-run) and queue-level "AI
  re-grade all pending" (enqueues every eligible request for an assessment, skipping
  ones that already have an AI record). Both are TA-clickable only — students cannot
  cause spend. The batch endpoint (`POST /api/regrades/ai-regrade-all`) accepts
  `dry_run` (JSON body or `?dry_run=1`): computes and returns the same
  `{enqueued, skipped, estimated_cost}` numbers a real call would, including running the
  monthly-budget check so a would-be 409 surfaces before confirming, but enqueues
  nothing and writes no audit row. Per-run cost caps do not apply to these single-leaf
  jobs, but the monthly budget gate does (D36), and unpriced models show "unknown," never
  a fake $0.

## D53 — AI re-grade pins the contested record's own method version — `v0-default` *(2026-07-03 morning round)*

- The AI re-grade job uses the method version pinned on the contested official record
  (the same models the original grade used), keeping the old-vs-new comparison
  apples-to-apples. If that method's provider has been removed, the request surfaces
  `regrade_requests.ai_error = "AI unavailable — provider removed"` (a visible terminal
  state, not a silent failure) and stays manual.

## Post-review deltas (D41-D53, resolved before merge)

Fixes made to the D41-D53 implementation during the same round's review pass, before
these were flagged as `v0-default` above — recorded here since they change what shipped
from the spec's first draft:

- **`answer_id` dropped from publish snapshots.** An earlier version of `SnapProblem`
  persisted `answer_id` so the report-attachment send job could resolve source images
  without a live query. Persisting it broke the D30 changed-only-republish diff:
  snapshots stored before the field existed decode with `AnswerID` zero-valued and
  re-marshal as `"answer_id":0`, so every pre-existing student's snapshot would
  byte-diff as "changed" across that one upgrade and re-email the whole cohort. Fixed by
  dropping the field; the send job instead resolves each problem's answer id **live**
  from `(student_id, problem number)` at send/resend time
  (`Sender.resolveAnswerID`) — answers are pre-materialized with a natural unique key,
  so the resolution is stable. Grade content still comes entirely from the snapshot;
  only the image-ref lookup goes live.
- **`MonthToDateCost` now counts `regrade_ai` spend.** The monthly budget query
  originally summed `cost_usd` only for `source='model'` records, so a stricter AI
  re-grade's cost (source `regrade_ai`, migration 0024) was invisible to the D36 budget
  gate — `ai-regrade-all` could push month-to-date spend over the configured cap with no
  409. Fixed: `MonthToDateCost` sums both `'model'` and `'regrade_ai'`.
- **`ai_error` is a visible-state column, not a log field.** Migration 0024 adds
  `regrade_requests.ai_error TEXT` so a terminal AI re-grade failure (provider removed,
  no contested record, malformed output past the re-ask cap) has somewhere to surface
  to the TA in the queue/detail UI — 0023 added `ai_record_id` for the success path but
  nothing carried the failure reason before this. Short constant strings only (never
  student/request text, per CLAUDE.md).
- **Terminal-vs-transient attachment build errors are classified, not uniformly
  retried.** `Sender.buildAttachment` originally let every failure (a missing font
  config, a missing blob store, a blob read error, a report-build bug) retry via River
  like any other transient send failure. A misconfigured `ADAMARKER_REPORT_FONT` or an
  unwired blob store will never succeed on retry, so those two preconditions now return
  a sentinel (`errTerminalAttachmentBuild`) the sender checks with `errors.Is` and routes
  straight to the terminal failure path instead of burning retry attempts; a genuine
  transient issue (blob read hiccup, in-flight report-build error) is left unwrapped and
  still retries as before.

### N3 fix wave (2026-07-03) — second whole-branch review

Four more deltas from the second review pass over the D41-D53 branch:

- **Single-request AI re-grade is now budget-gated (I2).** `handleAIRegradeAll` ran the
  D52 + OPERATIONS.md monthly-budget 409; `handleAIRegrade` (the per-request button) ran
  **none** — a request with no problem tag re-grades every officially-graded answer for
  the student, and `?rerun=1` re-spent indefinitely, all ungated. The estimate+MTD+409
  logic is now factored into ONE shared helper (`enforceAIRegradeBudget`) called from
  **both** handlers so they cannot drift; a new `RegradeRequestContestedAnswers` query
  prices one request's contested answers (with pinned provider/model) regardless of
  `ai_record` state, so re-runs are priced too. Decimal strings; "unknown" pricing fails
  open per D35, same as the batch.
- **Individual resend is live-batch-only; 409 on a superseded batch (I1) — D46
  narrowed.** D46 said resend covers a single item "regardless of status". That was too
  broad: on a **superseded** (unpublished) batch the send job's unpublish guard skips the
  re-enqueued send, so an unconditional resend silently downgraded a `sent` item to
  `pending` and wedged it there forever. `handleResendItem` now 409s when the item's
  batch is superseded (the row already carries the flag) — corrections to a superseded
  batch go through re-publish (unpublish → publish), not individual resend. The UI's
  superseded path becomes a disabled button + explanatory line. "Regardless of status"
  still holds *within* a live batch (pending/sent/failed/skipped).
- **The report "seam" is a package-function seam, not a `ReportRenderer` interface.**
  D42 and the spec name a `ReportRenderer` seam; no such interface exists. `internal/report`
  exposes plain package functions — `Build(fontPath, ReportInput)`, `BuildZIP(ReportInput)`,
  and `CheckFont(fontPath)`. It is a package boundary (the one place `go-pdf/fpdf` is
  imported), sanctioned like the other third-party seams, but there is no interface type
  to mock — callers depend on the functions directly. Recorded so the "seam" name isn't
  mistaken for an injectable interface.
- **Multi-answer AI re-grade writes one record per contested answer; the request links
  the last (as-built).** A regrade request with no problem tag covers every
  officially-graded answer for the student, so `RegradeAssistForRequest` appends one
  `source='regrade_ai'` grading_record **per contested answer** in a loop.
  `regrade_requests.ai_record_id` is a single FK, so it links the **last** record written;
  the detail UI therefore shows one AI re-grade even when several were produced. This is
  the shipped behaviour (not a per-answer fan-out in the UI) and is bounded by the number
  of the student's officially-graded answers in scope.

## D54 — Reply format is a strict `<pN>` tag contract, not prose parsing — `v0-default` *(2026-07-03 evening round, regrade v2)*

- One student reply = one turn = one request naming **all** contested problems in an
  exact, lowercase, ASCII format: `<pN>` … `</pN>` per problem, N the problem number
  as printed on the exam; everything between the tags is that problem's complaint
  text verbatim (multi-paragraph safe). This replaces the v1 free-text reply plus a
  best-effort EN/ZH problem-number guess (D49) — the guess is retired from the
  inbound path entirely (see D55).
- Parser is a pure, table-tested package (`internal/regrade/parse.go`,
  `ParseBlocks`): `>`-quoted lines are stripped before matching (so a quoted copy of
  our own template in a reply can never self-match), duplicate `<pN>` blocks for the
  same N concatenate in arrival order, and text outside all tags is ignored
  (greetings/signatures).

## D55 — No normalization, no fallback: malformed tags are silently ignored, and the v1 prose heuristic is retired — `v0-default` *(2026-07-03 evening round)*

- Exact match only: `<q1>`, full-width `＜ｐ１＞`, Cyrillic lookalikes, uppercase,
  inner spaces, or an unclosed `<p1>` without `</p1>` ⇒ that block is silently
  ignored — no normalization, no heuristic recovery, no notice. The format is the
  contract; violations are on the student. Unknown N (no such `problems.number` in
  the token's assessment) is likewise silently ignored.
- **`internal/regrade/match.go`** (the D49 EN/ZH/CJK-numeral prose-guessing parser)
  was dead code on the inbound path from this point on — nothing outside its own
  package/tests called it. The `PATCH /api/regrades/{id} {problem_id}` TA-editable
  chip endpoint this note originally cited as its last caller was itself removed in
  the same evening round's schema replace (v2's per-problem sub-items, D59,
  superseded the v1 single `problem_id` chip entirely). With that endpoint gone,
  `match.go` had zero callers anywhere in the tree and was deleted (whole-branch
  review F6) along with its tests.
- **Amendment (2026-07-10, UX-fixes round):** tag matching widened to full-width/case
  variants — `pTagPattern` now accepts full-width brackets/slash (＜ ／ ＞), full-width
  ｐ/Ｐ, uppercase `P`, and full-width digits ０-９ (normalized to the same problem
  numbers as ASCII), in any mix, because CJK input methods emit exactly these forms.
  Everything else in D55 stands: inner spaces, other scripts (Cyrillic lookalikes),
  unclosed tags, and unknown N are still silently ignored, and the complaint body text
  is still verbatim — normalization applies to the tags only, never the text between
  them. The grade email now also embeds the literal format template
  (`email.RegradeReplyFormatTemplate`, moved from httpapi) so students see the expected
  format before their first reply.

## D56 — Translation layer: deterministic `pN` → `problem_id`, no subject parsing — `v0-default` *(2026-07-03 evening round)*

- Token → `publish_item` → batch → assessment (unchanged from v1); `pN` → the
  assessment's `problems.number = N` → `problem_id` (unique per assessment) — no
  subject-line parsing anywhere in the chain. Recorded operator rule (known gap,
  unchanged): renumbering a published assessment's problems re-points any old
  tokens' `pN` meaning, so don't renumber after publish.

## D57 — Single-use per-turn token chain replaces the v1 count-based rate cap — `v0-default` *(2026-07-03 evening round)*

- Token v2: `v2.<publish_item_id>.<turn>.<expiry-unix>.<b64url HMAC-SHA256(...)>`,
  same HKDF subkey as v1 (`internal/email/token_v2.go`, `MintTokenV2`/`VerifyTokenV2`).
  v1 tokens are rejected outright — pre-production, no live tokens existed to
  migrate, so this is a replace, not a compatibility shim.
- The grade email carries token turn=1; result email #N carries token turn=N+1;
  confirmation and reminder emails carry **no token and no Reply-To** (replying to
  them physically cannot enter the pipeline).
- **Structural fix for the turn-assignment TOCTOU** flagged in the prior round
  (PLAN_GAPS: "two genuinely concurrent verified replies for the same (student,
  assessment) could race onto the same turn number" under the old `turn = 1 + prior
  verified count` read-then-write). v2 replaces the count-based turn computation and
  the v1 rate cap (`ADAMARKER_REGRADE_MAX` verified-count-per-pair) with a **partial
  unique index** `regrade_requests(publish_item_id, turn) WHERE kind = 'filed'`
  (migration 0025). A token is consumed exactly when a request with ≥1 valid block
  files against it — a race is resolved by Postgres, not application logic: the
  losing INSERT hits a `23505` unique violation and is recorded as an `addendum` row
  instead (dimmed under the filed request in the queue), no processing, no
  confirmation, no turn burned. `FileRegradeRequestV2` inserts the request row and
  its sub-items in one transaction (a within-round review fix — a two-step
  insert-then-attach-subitems sequence could otherwise strand a consumed slot with
  zero sub-items on a mid-sequence failure).
- All-garbage replies (0 valid blocks) do **not** consume the token — consuming
  without filing would end the chain with no result email and no next token,
  stranding the student with no recovery path (D58). Recorded as `unparsed`, total
  silence.
- Turn max stays `ADAMARKER_REGRADE_MAX` (existing env, default 3; unchanged name —
  see D62 for its v2 semantics), read at receipt time; in-flight tokens carry their
  own turn, so a mid-term change of the value stays coherent per-thread rather than
  retroactively reinterpreting outstanding tokens.

## D58 — All-garbage replies never burn a turn — `v0-default` *(2026-07-03 evening round)*

- A reply with 0 valid `<pN>` blocks against a live token does not consume that
  token: consuming without filing would end the chain with no result email and no
  next-turn token, stranding the student with no way to retry. Recorded as an
  `unparsed` row (kind='unparsed'), the token stays live, and the system stays
  silent (no auto-notice — see D62 for the TA-clicked reminder that covers this
  case).

## D59 — Per-problem sub-items with a hard-gated, TA-clicked result send — `v0-default` *(2026-07-03 evening round)*

- Schema: `regrade_requests` keeps one row per inbound email; a new
  `regrade_request_problems` table holds one sub-item per contested problem
  (`request_id, problem_id, complaint_text, ai_record_id, ai_error, verdict
  upheld|regraded|NULL, verdict_note, verdict_by, verdict_at`, `UNIQUE(request_id,
  problem_id)`). The v1 single `problem_id`/`ai_record_id` columns on
  `regrade_requests` are dropped (migration 0025). AI assist re-scopes to one
  sub-item per job — the LLM sees only that problem's masked pages, rubric,
  reference, original grades, and that problem's complaint text; the v1 "no problem
  tag ⇒ fan out to every officially-graded answer" behavior (D53's N3 note) is
  deleted along with it. TAs may still manually **add** a sub-item on a **filed**
  request (`POST /api/regrades/{id}/problems`, escape hatch, audited) — never on
  unparsed rows. There is no edit/delete-sub-item endpoint; a "correction" is done
  by adding the right problem (if missing) and upholding the wrong one via the
  normal per-problem verdict, not by editing an existing sub-item in place.
- Result email is **TA-clicked** (`POST /api/regrades/{id}/send-result`), not
  auto-sent, and hard-gated server-side: 409 until every sub-item has a verdict
  (`AllProblemsVerdicted` — zero sub-items does NOT vacuously pass). The 409 body is
  structured (`{error, unverdicted:[{problem_id, problem_number}]}`) so the UI
  renders the per-problem checklist authoritatively rather than re-deriving it
  client-side.
- **Send-once is enforced by an atomic flip-before-send, not a pre-check read**
  (a within-round CRITICAL review fix, `handleSendResult` in
  `internal/httpapi/regrade.go`): the handler resolves the request (flips it out of
  `received`/`under_review`) in the same guarded step that wins the right to send,
  BEFORE the provider send call — closing the window where two concurrent
  `send-result` calls could both pass a plain status pre-check and both attempt to
  send. A send failure after winning the flip leaves the request resolved with no
  email delivered (recorded, not retried into a second send) rather than reopening
  the once-only window.
- Result email #N carries per-problem sections (quoted complaint → outcome word → TA
  note → new score when regraded, sourced from the CURRENT official total, never a
  superseded snapshot — same principle as the N2 addendum's D34 fix), an attempt
  counter, and the next-turn token as Reply-To; the final turn (#MAX) instead gets a
  handoff token and "this was your final attempt" copy. Sending resolves the request
  (`resolved`), audited `regrade.send_result`.

## D60 — TA-per-problem assignment and final-turn handoff — `v0-default` *(2026-07-03 evening round)*

- `problem_ta_assignments(problem_id UNIQUE, user_id TA+, assigned_by, assigned_at)`
  — at most one TA per problem, one TA may own many. UI: a picker on the
  assessment's problems editor; `GET /api/assessments/{id}/ta-assignments` and
  `GET /api/graders` (lecturer+) back it. Publish preview warns (not blocks) on
  unassigned problems.
- Consuming the handoff token (a reply to result #MAX with ≥1 valid block) records
  the request as `handed_off`; per contested problem, the assigned TA receives an
  email with the assessment/problem, student name+ID+email, that problem's
  complaint text, the student's full prior-turn verdict history for that problem,
  and an app deep link — one email per (TA, student) covering every contested
  problem that TA owns. This is a deliberate PII-to-authorized-grader carve-out
  (same trust class as grade mail). Problems with no assigned TA in a handoff
  request are flagged "no TA assigned" (lecturer-visible); no email is sent for
  them (no target). Audited `regrade.handoff` per notified TA.
- Implementation detail (race-safety): the handoff insert follows the exact same
  pattern as a normal filing — insert as `kind='filed'` first, winning the
  `(publish_item_id, turn)` slot via the same partial unique index — THEN flip to
  `handed_off` via `MarkRequestHandedOff`. Inserting directly as `handed_off` would
  bypass the index's `WHERE kind='filed'` guard and let two concurrent final-turn
  replies both "win" the handoff.
- After handoff the system is permanently silent for that thread: no further tokens
  are issued; anything else inbound records as an addendum.

## D61 — Handoff email is a deliberate PII-to-authorized-grader carve-out — `v0-default` *(2026-07-03 evening round)*

- The TA-notify email at final-turn handoff (D60) carries student name, student ID,
  student email, and the contested problem's complaint text plus prior-turn verdict
  history to the assigned TA's own mailbox. This is the same trust class as the
  existing grade-result email (a TA is an authorized grader, not a third party) —
  the first internal-mail path that carries student data outside the
  masked-image/redacted-text discipline the AI-assist paths use (D19/D51). Complaint
  and history text are never logged (only counts/ids), per CLAUDE.md.

## D62 — Unparsed rows get a TA-clicked, anchored, structurally reply-proof reminder — `v0-default` *(2026-07-03 evening round)*

- Unparsed rows (0 valid blocks, live token — D58) surface under an "Unparsed" queue
  filter, dimmed, showing student/assessment/which-email-of-the-chain and the raw
  text. A **Send reminder** button (TA+, once per row — disabled after send, audited
  `regrade.remind`) sends an email that names the assessment, the exact subject and
  sent date of the email whose token is still live, states the attempt was NOT used,
  includes the literal `<pN>` format template, and tells the student to reply **to
  that result email, not to this reminder**.
- The reminder itself carries no token and no Reply-To — structurally reply-proof;
  any reply to it lands in the plain mailbox, never re-entering the pipeline. It is
  never sent automatically, preserving the existing no-backscatter posture;
  discretion to nudge a confused student stays with the TA.

## D63 — The page is the staging unit — `v0-default` *(2026-07-04 page-level scan intake)*

- One `scan_pages` row per physical page (spec
  [`2026-07-04-page-level-scan-intake-design.md`](superpowers/specs/2026-07-04-page-level-scan-intake-design.md)
  §2, §4), replacing the file-level `scan_files` model (one file = one student's
  whole paper). Every page is rendered, its three header regions cropped and OCR'd,
  and it is either auto-assigned to a (student, problem) cell or parked in an orphan
  queue. Finalize promotes each assigned page through the existing D22 per-problem
  image seam (`ingest.Ingest(Kind: "image", TargetProblemID: …)`) — supersede chain,
  graded-guard, and `submissions_active_problem_uniq` unchanged. The old file-level
  flow (per-file candidate student, positional page→problem mapping, ReviewStrip
  serial confirm) is deleted, not kept alongside; the Submissions tab's direct
  `<student_id>.pdf` path is untouched.
- Structural refinement beyond the spec's §4 schema: a batch can carry **several**
  source files (several PDFs in one upload, or a zip's entries), tracked in a new
  `scan_sources` table (`batch_id` FK, one row per uploaded PDF/image, its own
  `source_sha256` for duplicate-source detection). Page-split idempotency is
  therefore `UNIQUE (source_id, page_index)` — per source — not the spec's
  per-batch `UNIQUE (batch_id, page_index)`; two sources in the same batch both
  legitimately have a "page 0". `scan_batches.finalized_at` is dropped outright:
  finalize is assessment-wide and incremental (§7 item 5), so no single batch ever
  "finishes."

## D64 — ID+name independent agreement, fail-safe problem parsing — `v0-default` *(2026-07-04 page-level scan intake)*

- Auto-assign (spec §6) requires the student-ID read and the name read to
  *independently* resolve to the same live (non-withdrawn) roster student.
  Student ID is exact-only (normalized trim/case-fold/space-strip, never fuzzy — a
  one-digit OCR error is exactly how a page lands on the wrong real student). Name
  uses the existing conservative `match.go` rungs; an illegible or ambiguous name
  never auto-assigns even with a clean ID — the page orphans with the ID-matched
  student pre-filled (`proposal_source = 'ocr_id'`).
- ID→student-A / name→student-B disagreement is its own proposal source,
  `ocr_disagree` (`internal/scan/match.go`), surfaced distinctly in the orphan queue
  as a possible wrong-ID-written case, with **no pre-fill** (neither guess is
  trusted over the other).
- Problem-reference parsing (`internal/scan/problemref.go`) is fail-safe by
  construction: NFKC-fold, strip an accepted prefix (`Q`/`P`/`q`/`p`/`問`/`第`/`#`),
  read up to 3 digits, strip an accepted suffix (`.`/`)`/`:`/`題`), and require full
  consumption of the input — any leftover character (garbage, a second number, an
  unrecognized prefix) fails the parse rather than guessing. Out-of-range numbers
  (no matching problem in the assessment) also fail. A failed parse orphans the
  page (with the student pre-filled if the student rung passed).

## D65 — Occupied cells are never overwritten; promotion state is derived from live links — `v0-default` *(2026-07-04 page-level scan intake)*

- Auto-assign only fills **empty** cells (spec §6): a page resolving to an occupied
  cell parks as `duplicate` (identical `image_sha256` — no action needed, collapsed
  in the UI) or `conflict` (different content — side-by-side keep/replace/discard
  chooser). Manual assignment onto an occupied cell prompts the same way. Replacing
  a **graded** cell always routes through ingest's existing force-replace guard and
  is an explicit human action; auto-assign can never touch graded work.
- Implementation refinement not spelled out in the spec: whether a cell counts as
  "occupied" is derived from whether the linked submission is still **live** —
  `livePromotedQ` (`internal/scan/mutations.go`) checks the current state of the
  page's `submission_id` link rather than trusting the stored pointer. A page
  linked to a submission that has since been retracted or superseded no longer
  wedges the cell; mutations heal stale links on write instead of leaving a ghost
  occupant. `ResolveConflict`'s replace action is crash-recoverable by simply
  re-running it (idempotent against a partially-applied prior attempt).
- The graded-guard rejection on a forceless replace maps to HTTP 400 via a
  sentinel, `scan.ErrInvalidInput` (`internal/httpapi/scans.go`), rather than a
  generic 500 or a silent no-op — so the UI can detect "this needs force" and
  offer the explicit force checkbox instead of a dead end.

## D66 — Finalize seeds identity mask regions onto every page — `v0-default` *(2026-07-04 page-level scan intake)*

- Every page carries identity at known coordinates (the three header regions), so
  `Finalize` seeds per-assessment `mask_regions` (`page_scope='all'`) from the
  `student_id` and `name` `id_regions` — never `problem_id`, which the grader may
  legitimately need to see (spec §8). This feeds the existing mask pipeline so
  identity is masked on every page before AI grading, not just where a region was
  hand-drawn per-assessment.
- Seeding is **append-only** (comment-pinned in `internal/scan/finalize.go`,
  `seedMaskRegions`): it dedupes by exact rect equality but never removes rows for
  a region that was later edited or redrawn. Editing an id-region after a finalize
  leaves the old mask rect in place alongside the new one — draw regions final
  *before* finalizing; the adjust-and-refinalize workflow is not yet supported and
  would need revisiting this function if it becomes real.

## D67 — Old scan staging dropped in migration 0029; regions must be redrawn — `v0-default` *(2026-07-04 page-level scan intake)*

- Migration `0029_page_level_scan_intake.sql` (not the 0010-era `scan_files`
  table's original migration) drops `scan_files` outright, drops `scan_batches`'
  `problem_id` scoping and `finalized_at`, renames `zip_ref`→`source_ref`, adds the
  new `scan_sources`/`scan_pages` tables, and retypes `id_regions` with a `kind`
  CHECK (`student_id`/`name`/`problem_id`), dropping the old `page_index` column.
  Existing `id_regions` rows carry no `kind` and are deleted by the migration —
  **regions must be redrawn once** after upgrading. Any batch still staged in
  `scan_files` at migration time is lost; in-flight batches must be drained
  (finalized or discarded) before deploying (solo-deployment assumption, called out
  in the migration header).
- `0029`'s Down deliberately does **not** recreate `scan_files_batch_idx`: `0013`'s
  Down already recreates that index later in the same down-walk (0013 dropped it
  in the Up direction as redundant with `UNIQUE(batch_id, source_sha256)`), so
  `0029` doing it too would collide (`relation already exists`, SQLSTATE 42P07)
  when walking all the way down to 0.

## D68 — Streamed transport, chunked rendering, retired-shape-proof job kinds, local-only region template — `v0-default` *(2026-07-04 page-level scan intake)*

- Scan-batch uploads are **streamed to the blob store**, not buffered (spec §5):
  `scan.MaxSourceBytes` is a package `var` (2 GiB, `2 << 30`) bounding one uploaded
  scanner PDF — a var rather than config so tests can shrink it; flag if a runtime
  knob is wanted. The zip archive cap (`MaxZipBytes`) is likewise 2 GiB; the
  per-entry/loose-image cap (`MaxEntryBytes`) stays at ingest's existing 50 MiB.
- Rendering is chunked: `renderChunkSize = 25` pages share one PDFium open
  (`internal/scan/service.go`), so a 2,000-page scan run doesn't re-open the
  document per page (mirrors ingest's open-once rule).
- River job kinds were renamed rather than reusing the old ones: `scan.split`,
  `scan.render_pages`, `scan.identify_page`, `scan.promote_page`
  (`internal/queue/river.go`) — distinct strings from any retired file-level job
  kind, so a stale/retired arg shape already sitting in the queue at deploy time
  can never mis-decode into the new job body.
- The Identify tab's region editor (`frontend/src/components/identify/IDRegionCard.tsx`)
  can draw the three regions against a **local-only template image**: picked from
  the user's device, held only as a `URL.createObjectURL` object URL, and never
  uploaded or fetched anywhere. This breaks a circularity — regions must exist
  before any batch can render pages to serve as a sample, but a template picked
  before any batch exists can't itself depend on the batch pipeline.

---

## 2026-07-20 — PDF-aligned simplification (D69)

**D69 — Calibration sample runs: a fourth run scope `sample` whose `scope_id` is N.**
(Spec [`2026-07-20-pdf-aligned-simplification-design.md`](superpowers/specs/2026-07-20-pdf-aligned-simplification-design.md);
migration `0037_sample_scope.sql`.) The TA guide's calibration batch (§3.1) used to require
one answer-scoped run per sampled answer; a `sample` run draws a deterministic,
problem-stratified `min(N, pool)` at PLAN time (`grading.SelectCalibrationSample`, seeded by
run id — the spot-check sampler's idiom, deliberately duplicated rather than factored so
D37's canonical-sample determinism cannot be perturbed) and persists it as ordinary run
items, making the draw recorded, reproducible, and re-plan-idempotent. Flagged defaults:

- **Gates run over the whole assessment pool** (mask gate, rubric/refsol blockers,
  `no_rubric_problems`) since the draw may touch any problem — conservative by design.
- **Cost preview and the D36 pre-flight budget estimate use `min(N, pool)`**, not the pool.
- **Sample runs cannot be pinned as the final source** — already enforced structurally by
  `ErrFinalRunNotAssessmentScope`; they are probes for the Analysis method cards.
- **Down-migration note:** `assessments_final_run_fk` (0035) is `DEFERRABLE INITIALLY
  DEFERRED`, so 0037's down must `SET CONSTRAINTS ALL IMMEDIATE` between deleting sample
  runs and altering the CHECK, or the DDL fails with pending trigger events (SQLSTATE
  55006) — regression-tested with data present (`TestMigration0037_DownWithSampleRunData`),
  which the empty-DB `TestMigrations_UpDownUp` structurally cannot catch.

Same round, UI: Overview's workflow card now mirrors the guide's §9 stages (new
"Calibrate on a sample" and "Handle regrades" steps), ProblemReview gains the §6.3
"By score" side-by-side view (masked variant only — an unmasked or pageless answer gets an
explicit placeholder, never the original image), and user-visible "ADA-Marker" strings
became "AdaGrade" (operational `adamarker`/`ADAMARKER_*` identifiers unchanged).

---

## 2026-07-20 — Typst result-PDF renderer (D70)

**D70 — LaTeX math in student-facing output finally renders: an optional Typst
renderer for the result PDF.** (Spec
[`2026-07-20-typst-report-design.md`](superpowers/specs/2026-07-20-typst-report-design.md).)
The grading template mandates "LaTeX for math" (D5's transcribe-then-grade), but no
surface ever rendered it — students got raw `\frac{...}` in comments. With
`ADAMARKER_TYPST_BIN` set (and the existing `ADAMARKER_REPORT_FONT` attachments gate on),
PDF attachments are rendered by Typst with LaTeX math typeset via the pinned
`@preview/mitex` package; any compile failure falls back to fpdf, so sends never fail on
this. Flagged defaults:

- **Injection hardening**: comments are model/TA text derived from student answers, so
  the generated `.typ` source keeps ALL user text inside escaped string tokens
  (`internal/report/typstmarkup.go`'s auditable invariant) — Typst directives in a
  comment render as literal text — and compiles run under `--root <tempdir>`.
- **Runaway-compile kill** (adversarial review, high — reproduced): a self-referential
  LaTeX macro inside a math span drives mitex's expander into unbounded recursion — a
  HANG, not a compile error, so the fpdf fallback would never run and the single-worker
  email queue would wedge. The compile subprocess now runs under the caller's context
  plus a 20s hard timeout (`exec.CommandContext` + `WaitDelay`), regression-tested with
  the macro bomb.
- **Determinism**: `--creation-timestamp 0` keeps builds byte-identical for identical
  input (the fpdf invariant), regression-tested.
- **Disclosure unchanged**: every attachment shape now renders exactly what the email
  already discloses — per-criterion name/score plus the problem-level comment, wired
  into the Typst PDF, the fpdf PDF, and the ZIP grades.txt alike (`ProblemReport` gained
  the missing `Comment` field; on non-Typst paths LaTeX stays raw source). Per-criterion
  AI rationales stay out of student output on all surfaces.
- **PII**: typst stderr is suppressed in errors (compiler diagnostics quote source lines,
  which embed comments).
- **Ops**: mitex is fetched once into the local Typst package cache (network on first
  compile per machine, or pre-seed the cache) — noted in `.env.adamarker.example`.

---

*Not yet decided here (still open in PLAN_GAPS): bounce/complaint webhook handling,
retention/erasure (B-H7), TA data scoping (B-M15), batch-vs-sync threshold (D11's
deferred batch APIs), cross-exam reports (Phase 8 remainder), partial-cohort
`not_ingested` over-blocking (a roster student added mid-term with no intent to submit
still blocks publish for the whole assessment — no per-student exemption exists yet).
B-H17 (post-ingest reconciliation + fast masked-crop review) is now partially
addressed — the assignment matrix (spec §7 item 3) gives the missing/conflict
cross-problem view B-H17 asked for, and D66 masks identity on every page rather than
by per-assessment sample — but the fast keyboard-navigable masked-crop accept/flag
review and the modeled flag lifecycle it also asked for remain unbuilt.*
