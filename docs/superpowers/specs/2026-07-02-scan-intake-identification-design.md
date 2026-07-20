# Scan intake & student identification — design spec

*2026-07-02. Extends Phase 2 ingestion (spec §7, D1/D10/D13) for the exam workflow the
plan's §12 left open ("exact submission format and collection method"). Decisions
D18–D23 in [`DECISIONS.md`](../../DECISIONS.md) carry the flagged defaults.*

## 1. Problem

Assignments arrive as one PDF per student named `<student_id>.pdf` — the existing
pipeline handles them. **Exams don't.** ~200+ papers are scanned one by one into files
with scanner-generated names (PDFs, or single-page images), possibly zipped. Nobody is
sorting those by hand. Also unhandled today: files that are **images** (one page each,
one problem each), **missing submissions**, and **withdrawn students**.

The workflow the TAs need:

1. Upload a zip (or many loose files) of scans for an assessment.
2. Draw the **ID region** — the box on the paper where students write their student ID
   and name (usually Chinese) — with the same drag UX as mask regions.
3. The system renders each file's first page, **crops just that region**, OCRs it with
   a cheap VLM, and proposes a roster match.
4. A TA pages through the crops (keyboard-paced, crop + proposed student per row),
   confirming or correcting each. **Every file must be human-confirmed** before
   anything becomes a gradable submission; assignments remain recheckable after.
5. Reconciliation shows which active students still have no paper (missing) and keeps
   withdrawn students out of the way.

## 2. Approach (D18): a staging pipeline in front of the existing one

Uploaded scans live in **staging tables** (`scan_batches`, `scan_files`) that never
touch the graded domain. When the TA finalizes a batch, each confirmed file is
**promoted through the existing ingestion tail** — the same code path `AssignQuarantine`
already uses — so every existing guard (supersede chain, force-if-graded,
published-never, answer materialization, positional mapping) applies unchanged at one
boundary.

Rejected alternatives: making `submissions.student_id` nullable (breaks the
active-unique index and every consumer; misassignment rollback would mutate graded
data), and overloading `upload_quarantine` (per-file exception path; no batch concept,
no finalize gate, no image support).

```
upload (zip / files)                                staging                    graded domain
──────────────────────  scan.expand ─▶ scan_files ─ scan.render ─▶ page-0 JPG + ID crop
                                                    scan.identify ─▶ OCR → roster match → proposal
                        TA confirms / corrects / discards each file  (Identify tab)
                        finalize ─▶ per file: ingest.IngestFile(...) ─▶ submissions / answers / pages
```

## 3. Schema (migration 0010)

**Core widening** (needed by any approach):

- `submissions`: rename `pdf_ref → source_ref`, `pdf_sha256 → source_sha256`; add
  `source_kind TEXT NOT NULL DEFAULT 'pdf' CHECK (source_kind IN ('pdf','image'))`;
  add `problem_id BIGINT NULL` (composite FK `(problem_id, assessment_id)` →
  `problems`, matching `answers`) for per-problem image submissions (D22); add
  `retracted_at TIMESTAMPTZ` (unassignment tombstone, §7). The active-unique index
  becomes two partial indexes:
  - whole-assessment: `(assessment_id, student_id) WHERE superseded_by IS NULL AND retracted_at IS NULL AND problem_id IS NULL`
  - per-problem: `(assessment_id, student_id, problem_id) WHERE superseded_by IS NULL AND retracted_at IS NULL AND problem_id IS NOT NULL`
- `students`: add `withdrawn_at TIMESTAMPTZ` (D23).

**Staging** (facts, not lifecycle enums — status derives per D2):

- `scan_batches` — id, `assessment_id` FK, `problem_id` NULL (composite FK; set ⇒ every
  file in the batch is a single-page submission for that problem), `ocr_enabled BOOL`,
  `ocr_provider TEXT`, `ocr_model TEXT`, `zip_ref TEXT` (when uploaded as zip),
  `finalized_at`, `created_by`, `created_at`.
- `scan_files` — id, `batch_id` FK CASCADE, `original_filename`, `source_ref`,
  `source_sha256`, `source_kind ('pdf','image')`, `page_count INT`,
  `page0_image_ref`, `page0_width/height INT`, `id_crop_ref`,
  `ocr_student_id TEXT`, `ocr_name TEXT`, `ocr_legible BOOL`,
  `proposed_student_id BIGINT NULL FK students`,
  `proposal_source TEXT NULL CHECK IN ('filename','ocr_id','ocr_fuzzy','ocr_name')`,
  `assigned_student_id BIGINT NULL FK students`, `assigned_by`, `assigned_at`,
  `discarded_at`, `discard_reason TEXT`,
  `submission_id BIGINT NULL FK submissions` (set at promotion),
  `error TEXT`, `attempts INT`, timestamps.
  `UNIQUE (batch_id, source_sha256)` — duplicate scans in one batch collapse;
  idempotency key for River redelivery.
  Derived display state: `error → discarded → promoted → assigned → proposed →
  unidentified → processing`.
- `id_regions` — mirrors `mask_regions` (per-assessment normalized 0..1 rects,
  PUT-replaces-all) with `page_index INT DEFAULT 0` instead of `page_scope`; all
  rects must share one `page_index` (validated on PUT — one crop source page). Kept
  separate from `mask_regions` because `ApplyMasks` blindly paints every mask region —
  and note the pleasant symmetry: the ID box a TA selects here is usually exactly what
  they should *also* add as a mask region for grading (UI offers copy-to-mask).

OCR text columns hold PII; that's acceptable (the DB already holds the roster) — the
rule (D14) is it never reaches logs, job args, or error strings.

## 4. Privacy: identity ⊕ answer content (D19)

Grading sends **answer content with identity masked out** (D10). Identification sends
**identity with no answer content** (a tight crop of the ID box). The invariant
becomes: *a provider request carries identity XOR answer content, never both.*

Mechanically: `llm.Request.Images` changes from `[]imaging.MaskedImage` to
`[]imaging.ProviderImage` — a **sealed interface** (`JPEG()`, `SHA256()`, plus an
unexported method) implemented only by `imaging.MaskedImage` and the new
`imaging.IDCrop`. `IDCrop` is constructible only via `imaging.Crop(originalJPEG,
region, quality)` and `imaging.LoadIDCrop(key, bytes)` (gated on an `/idcrop/` key
segment, like `LoadMasked`'s `/masked/` gate). Sending an arbitrary unmasked page
remains a compile error.

This is a deliberate, documented carve-out: the ID crop (name + student ID) *is* PII
and *is* sent to a third-party model, because identifying 200 papers is the point.
Escape hatch: `scan_batches.ocr_enabled = false` runs the same pipeline with no
provider calls — TAs assign purely by eye from the crops.

## 5. Jobs (River)

House patterns copied from `run.plan`/`run.leaf`: args carry IDs only; transactional
enqueue with the row insert; idempotent workers keyed on natural uniques; terminal
failures set `scan_files.error` instead of erroring forever.

- `scan.expand` (queue `scan`, MaxWorkers 2 — CPU/IO bound): unzip `zip_ref` (skip
  `__MACOSX`, dotfiles, dirs; accept `.pdf/.png/.jpg/.jpeg`), create `scan_files`,
  store each entry's bytes, enqueue `scan.render` per file. Loose-file uploads create
  `scan_files` directly in the upload handler and enqueue `scan.render` the same way.
- `scan.render` (queue `scan`): PDF → `PageCount` + `RenderPage(p)` where `p` is the
  id-regions' shared `page_index` (0 when none defined, clamped to the document);
  image → decode (stdlib png/jpeg), downscale to `MaxLongEdgePx`, re-encode JPEG q85,
  `page_count=1`. Store `page0_image_ref`; crop each `id_region` rect →
  single `id_crop_ref` (rects cropped individually and stacked vertically into one
  JPEG when multiple); enqueue `scan.identify` if `ocr_enabled`. **No id_regions ⇒ no
  crop and no OCR** — the full page must never go to a provider (it would carry
  answer content, §4); the UI shows page 0 for manual assignment instead.
- `scan.identify` (queue `llm` — shares provider rate limiting): resolve
  `(ocr_provider, ocr_model)` via `llm.ProviderSource`, call `Grade` with the crop,
  strict schema `{student_id: string, name: string, legible: boolean}`, temperature 0,
  reasoning off, max_tokens 256; then roster-match in Go (§6) and write the proposal.
  Provider/model default: the batch form defaults to the first enabled provider and a
  cheap vision model (docs/MODELS.md; e.g. `qwen/qwen3.5-flash-02-23` on OpenRouter —
  per-crop cost is ~1/10 of a grading call; 200 files ≈ $0.05).

`queue.New` grows a deps struct (`*grading.Runner` + `*scan.Service`).

## 6. Roster matching (D21) — pure Go, test-first

Normalization: student IDs — NFKC fold (full-width → ASCII), uppercase, strip
non-alphanumerics; names — NFKC, strip all whitespace (Chinese names are often written
with gaps), case-fold for Latin.

Match ladder (first hit wins; proposal only, never auto-assign):

1. `filename` — normalized filename stem exactly matches a roster ID (D13 convention
   still works when files *do* have meaningful names). If OCR later disagrees, the file
   is flagged `conflict` for the TA.
2. `ocr_id` — normalized OCR ID exactly matches a roster ID.
3. `ocr_fuzzy` — unique roster ID within Levenshtein distance 1 of the OCR ID, and
   the OCR name matches that student's normalized name.
4. `ocr_name` — normalized OCR name matches exactly one roster student.
5. none — file shows as *unidentified*; TA assigns manually.

Withdrawn students are excluded from matching candidates (a withdrawn student's stray
paper surfaces as unidentified, which is correct — a human should look).

**Human confirmation is always required** — a proposal is never terminal. Keyboard
review makes confirming 200 exact matches a few minutes' work; correctness of exam
identity is worth it. Two files confirmed to the same student is blocked at assign
time (409 with the competing file) — the TA discards or reassigns one first.

## 7. Finalize, reconciliation, corrections

**Reconciliation** (`GET /api/scan-batches/{id}`): per-file rows (crop, proposal,
state) plus a roster panel — *active* students with no assigned file (**missing**),
withdrawn students (grayed, excluded from missing), and duplicate/conflict warnings.

**Finalize** (`POST /api/scan-batches/{id}/finalize`): requires every file terminal
(assigned or discarded); if any active student is missing it demands
`{"ack_missing": true}` and records the acknowledged list in the audit log. Then each
assigned file is promoted via the existing ingest tail — `IngestInput{Filename:
"<student_id>.<ext>", Data, Kind, TargetProblemID: batch.problem_id}` — creating the
submission + pages + answers under all existing guards. Per-file failures are recorded
on the row; finalize is idempotent and re-runnable (skips already-promoted files);
`finalized_at` is set when all promotions succeed. Missing students need no new
machinery: D1 pre-materializes their answers with zero pages ⇒ `no_submission`.

**Recheck / corrections** stay available after finalize:

- *Reassign* (`POST /api/scan-files/{id}/reassign {student_id}`): retracts the wrong
  student's submission (`retracted_at`, pages of that submission deleted — same
  scoped-deletion as the supersede path; blocked without `force` if graded, blocked
  always if published) and promotes the file for the right student in one action.
- *Unassign back to staging* works the same way without the re-promotion.

Page-deletion scoping changes from "all pages for (assessment, student)" to "pages of
the superseded/retracted submission" — equivalent for whole-assessment submissions,
and required for per-problem submissions (D22).

## 8. Image & per-problem submissions (D22)

A `scan_batches.problem_id` batch means each file is **one page for that one problem**
(the exam-scanned-per-question / image-per-question case). Promotion maps the single
page to that problem (not positionally), and the submission row carries `problem_id`,
so a student legitimately holds several live submissions per assessment (one per
problem) — the new partial indexes enforce uniqueness per scope. Supersede/force/
published guards evaluate within the same scope (that problem only). Whole-assessment
and per-problem submissions may coexist; a per-problem promotion supersedes only that
problem's prior pages. Multi-page PDFs in a per-problem batch: all pages map to that
problem (ordered `page_index` — answers spanning pages stay first-class per D1).
`GET /api/submissions/{id}/pdf` becomes `/source` semantically: streams with the
correct Content-Type by `source_kind` (route kept for compatibility).

## 9. Withdrawn students (D23)

`students.withdrawn_at` set/cleared via `PATCH /api/students/{id}` (lecturer+, audit-
logged). Effects: excluded from `MaterializeAnswers`, from the missing-list and ingest
report expected set, from matching candidates, and (Phase 6) from publish. Existing
answers/records are kept untouched (history). CSV re-import never toggles withdrawal —
it's an explicit UI action, so a re-import can't silently resurrect or drop anyone.

## 10. HTTP API

Staging (all authenticated; mutations CSRF-headered; TA role suffices except where noted):

- `POST /api/assessments/{id}/scan-batches` — multipart: `files[]` (many pdf/png/jpg)
  or a single `.zip`; fields `problem_id?`, `ocr_enabled?`, `ocr_provider?`,
  `ocr_model?`. Returns the batch. Caps: 50 MiB/file (existing), 1 GiB/zip.
- `GET /api/assessments/{id}/scan-batches` — list with derived progress counts.
- `GET /api/scan-batches/{id}` — files + reconciliation panel (§7).
- `POST /api/scan-batches/{id}/finalize` — body `{ack_missing?: bool, force?: bool}`.
- `POST /api/scan-files/{id}/assign|unassign|discard|reassign|retry`.
- `GET /api/scan-files/{id}/crop` · `GET /api/scan-files/{id}/page` — JPEG streams.
- `GET|PUT /api/assessments/{id}/id-regions` — mirror of mask-regions.
- `PATCH /api/students/{id}` — `{withdrawn: bool}`.

## 11. Frontend

New **Identify** tab in `AssessmentDetail` (between Submissions and Masking), standard
`{assessmentId}` prop pattern:

1. **ID region card** — the MaskingTab `RegionEditor` drag machinery extracted into a
   shared `components/RectEditor.tsx`, drawing on a sample scan page (or any uploaded
   file's page 0); "copy to mask regions" helper button.
2. **Upload card** — drop zip / many files; batch options (problem, OCR provider/model
   from the providers list, OCR toggle); per-batch progress (poll, like runs).
3. **Assignment review** — the MaskReviewPanel pattern: keyboard strip (`j/k` navigate,
   `Enter` confirm proposal, `e` edit → roster search box, `d` discard, `v` toggle
   full-page view), crop image left, proposal + roster picker right, dup/conflict
   badges, "pending only" filter, react-query cache patching to keep the cursor
   stable.
4. **Reconciliation + finalize card** — missing/withdrawn/conflict lists, finalize
   button with ack-missing confirm dialog.

Query keys: `["scan-batches", assessmentId]`, `["scan-batch", batchId]`,
`["id-regions", assessmentId]`; assignment mutations also invalidate
`["ingest-report", assessmentId]` + `["problem-summaries", assessmentId]` after
finalize/reassign.

## 12. Testing

House rules: new logic test-first; unit tests offline (fake renderer, local-disk
blobstore, `llm.StaticSource`/scripted fake provider — the existing fake fabricates
grading output from rubric schemas, so it gains a scripted mode for the identify
schema). Coverage targets: matcher table tests (normalization: full-width digits, CJK
names with spaces, Levenshtein ladder, withdrawn exclusion); `imaging.Crop` +
`IDCrop`/`LoadIDCrop` gates (and that `LoadMasked` still rejects `/idcrop/` keys and
vice versa); zip expansion (junk entries, nested dirs, caps); render normalization for
images; assign/conflict/discard/finalize state machine incl. idempotent re-finalize;
promotion guards (graded-without-force, published-never, per-problem scoping,
retraction); withdrawn-student effects; httpapi handler tests per the phase-test
pattern.

## 13. Explicitly out of scope (v0)

- Splitting one giant concatenated PDF containing many students' papers (upload shape
  is one file = one paper; a splitter can layer on later as a batch preprocessor).
- OCR of the *problem number* per file (per-problem batches carry the problem
  explicitly; mixed-problem image dumps are deferred).
- Per-file rotation correction UI (scanner output is assumed consistently oriented;
  flagged crops fall back to manual assignment).
- Bulk "accept all exact matches" (deliberate: per-file confirmation is the product's
  correctness stance; revisit only if review throughput actually hurts).
- Auto-verification OCR pass over *masked* grading images (B-C4) — unblocked by the
  `imaging.Crop`/OCR machinery built here, but a separate feature.

## 14. Deviations as built (2026-07-02, flagged post-review)

- §7 *unassign after promotion*: not built. `unassign` works only pre-promotion;
  the promoted-file correction path is `reassign` (retract + re-ingest), which
  covers the real flow. D18 documents only reassign — this spec overstated.
- §7 *"records the acknowledged list in the audit log"*: the audit row carries the
  **count** of acknowledged-missing students, not their identities — a deliberate
  D14 tightening (no PII in audit detail).
- A replacement paper for a student who already has a **live** submission cannot be
  `assign`ed inside a scan batch (conflict). Workaround: upload it as
  `<student_id>.pdf` via the Submissions tab, which supersedes under the D1 guards;
  a force-assign affordance can be added later if this bites.

## 15. Addendum: local OCR rung (D24, added 2026-07-02)

§5's identify step gained a fully-offline first engine: a ~11 MB PP-OCRv4
recognition ONNX model behind the `ocr.Reader` seam (internal/localocr,
onnxruntime ≥ 1.27). The ladder is now *filename → local OCR → cloud VLM →
human*; `ocr_enabled` gates only the cloud call, so with the local engine
configured the whole identification flow can run without student identity ever
leaving the machine — removing the §4/D19 carve-out in practice while keeping it
as an opt-in fallback for crops the small model can't read.
`scan_files.ocr_engine` records provenance. Setup: `make ocr-models` +
`ADAMARKER_OCR_MODEL/ADAMARKER_OCR_KEYS/ADAMARKER_ONNXRUNTIME`.
