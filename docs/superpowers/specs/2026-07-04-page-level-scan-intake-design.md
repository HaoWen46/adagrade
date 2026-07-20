# Page-level scan intake — design spec

*2026-07-04. Replaces the file-level scan staging of
[`2026-07-02-scan-intake-identification-design.md`](2026-07-02-scan-intake-identification-design.md)
(one file = one student's whole paper) with a page-native pipeline. The promotion
boundary into the graded domain (D18) and the per-problem image submission model (D22)
are kept and become the *only* path out of staging. Decisions D63–D68 below are
recorded in [`DECISIONS.md`](../../DECISIONS.md) — implemented, see that file for
what was actually built (including two structural refinements beyond this spec,
footnoted in §4 and §5 below).*

## 1. Problem

The 07-02 flow assumed each staged file is one student's complete paper. Reality at
250+ students: the whole pile is fed through a feeder and the scanner emits **one giant
multi-page PDF** — pages in arbitrary student/problem order, no per-student files, no
way to pre-sort without hours of manual work. The current model cannot represent this
input at all:

- `scan_files` is per-file with a single candidate student; a second file for the same
  student 409s assessment-wide (`ScanFileForStudentInAssessment` has no problem scoping
  and promotion never clears `assigned_student_id`).
- Only the identity page is OCR'd; interior pages are permanently unidentifiable.
- Whole-assessment promotion maps page *i* positionally onto problem *i* — a double-fed
  page silently shifts every later answer onto the wrong problem.
- Review is one-file-at-a-time serial confirmation; at ~2,000 pages that is unusable.

What makes a better model possible: **every page is self-identifying.** Students write
their name, student ID, and problem ID in fixed header boxes on every single page, and
**each problem's answer fits on exactly one page** (a second page for the same
(student, problem) is a mistake to flag, not a continuation).

## 2. Approach (D63): the page is the staging unit

One `scan_pages` row per physical page. Each page is rendered, its three header
regions cropped and OCR'd, and the page is either **auto-assigned** to a
(student, problem) cell or parked in an **orphan queue**. Finalize promotes each
assigned page through the existing `ingest.Ingest(Kind: "image", TargetProblemID: …)`
seam — the per-problem image submission path from D22, with its supersede chain,
graded-guard, and `submissions_active_problem_uniq` index unchanged.

Rejected alternative: keep the file-based promotion tail and synthesize one
per-student PDF from assigned pages ("pages in, files out"). It preserves the fragile
positional page→problem mapping underneath, forces per-student special-casing whenever
a problem is missing, and keeps the one-file-per-student model alive after we decided
to kill it.

The old file-level flow is **replaced, not kept alongside** (user call). The
Submissions tab's direct path (pre-organized `<student_id>.pdf` uploads, filename
match, positional mapping) is untouched as the alternative for already-sorted files.

```
upload (PDF(s) / zip of images)      staging (scan_pages)                 graded domain
─────────────────────────────  scan.split ─▶ one row per page
                               scan.render ─▶ page JPG + 3 region crops (chunked jobs)
                               scan.identify ─▶ OCR ─▶ match ─▶ auto-assign | orphan
                               orphan queue / conflict chooser  (Identify tab)
                               finalize ─▶ per page: ingest.Ingest(image, problem) ─▶ submissions/answers
```

## 3. Fixed inputs (user-confirmed)

- Scanner output: one (or a few) giant multi-page PDFs per run; zip-of-images stays
  supported (one image = one page, `scan.split` skipped).
- One page per (student, problem); duplicates are conflicts.
- Every page carries three handwritten header fields at fixed positions: problem ID
  (written prefixed: `Q1` / `P1` / `問1` style), name, student ID.
- Auto-assign policy: student ID **and** name must independently resolve to the same
  live roster student, and the problem number must be valid — anything less orphans.
- Re-uploads must never overwrite matched material; at most they fill missing cells.

## 4. Schema (new migration)

**`scan_pages`** (replaces `scan_files`, which is dropped):

> **Implemented as a refinement of the below** (D63): a batch may carry several
> uploaded source files (several PDFs in one upload, or a zip's entries), tracked
> in a new `scan_sources` table (`batch_id` FK, its own `source_sha256`). Page
> idempotency is therefore `UNIQUE (source_id, page_index)` — per source — not the
> per-batch `UNIQUE (batch_id, page_index)` described just below; two sources in
> the same batch each legitimately have a "page 0". See `DECISIONS.md` D63.

- identity: `batch_id` FK, `page_index INT` (position in the source; `UNIQUE
  (batch_id, page_index)` makes split idempotent), `image_ref`, `image_width/height`,
  `image_sha256` (rendered-page content hash for duplicate detection),
  `student_id_crop_ref`, `name_crop_ref`, `problem_crop_ref`.
- OCR reads: `ocr_student_id`, `ocr_name`, `ocr_problem`, `ocr_student_id_legible`,
  `ocr_name_legible`, `ocr_problem_legible` (BOOLEAN NULL = not yet OCR'd).
- scoping: denormalized `assessment_id` (copied from the batch), needed by the
  problem composite FKs and the cell-uniqueness index below.
- proposal: `proposed_student_id` FK students, `proposed_problem_id` (composite FK
  `(proposed_problem_id, assessment_id)` → problems, matching `answers`),
  `proposal_source TEXT CHECK (proposal_source IN ('ocr_agree','ocr_id','ocr_name'))`.
- assignment: `assigned_student_id`, `assigned_problem_id` (set together or not at
  all, enforced by a CHECK), `assigned_by` (NULL = auto-assigned, user id = manual),
  `assigned_at`.
- lifecycle: `parked_reason TEXT NULL CHECK (parked_reason IN ('duplicate',
  'conflict'))` + `parked_against BIGINT NULL REFERENCES scan_pages (id)` (the
  incumbent), `discarded_at`, `discard_reason`, `submission_id` FK (set on promote),
  `error`, `attempts`, timestamps.
- status stays **derived, never stored** (D2 pattern): error → discarded → promoted →
  parked(duplicate|conflict) → assigned → orphan (OCR done, no assignment) →
  processing.
- cell uniqueness: partial unique index on `(assessment_id, assigned_student_id,
  assigned_problem_id) WHERE assigned_student_id IS NOT NULL AND discarded_at IS NULL`
  — one live page per cell, mirroring `submissions_active_problem_uniq`.

**`scan_batches`**: drop `problem_id` scoping (obsolete — every page names its own
problem); `zip_ref` generalizes to `source_ref`. OCR provider fields stay.

> **Implemented as a refinement**: `finalized_at` is dropped from `scan_batches`
> too (not listed above) — finalize is assessment-wide and incremental (§7 item 5),
> so no single batch ever reaches a terminal "finished" state; a per-batch
> finalized timestamp would be meaningless. See `DECISIONS.md` D63.

**`id_regions`**: add `kind TEXT NOT NULL CHECK (kind IN ('student_id', 'name',
'problem_id'))`; exactly one live region per kind per assessment (validated on PUT);
drop `page_index` — regions are normalized 0..1 rects applied to **every** page.
Fixed UI color per kind.

## 5. Pipeline (River jobs)

- **upload** — `POST /api/assessments/{id}/scan-batches` accepts one or more PDFs
  and/or a zip of images. Parts are **streamed to the blob store**, not buffered
  (D68): per-file cap becomes configurable, default 2 GiB for scan batches (a
  2,000-page scan doesn't fit the 50 MiB direct-upload cap). Creates the batch row,
  enqueues `scan.split` per source file.
- **`scan.split`** — opens the PDF once, counts pages, inserts one `scan_pages` row
  per page (idempotent via `(batch_id, page_index)`), fans out `scan.render` in
  **chunks of ~25 pages per job** so each job re-opens the document once, not per page
  (ingest's open-once rule, F3). Images from a zip skip straight to per-page rows +
  render.
- **`scan.render`** — renders each page in its chunk to JPG, crops the three regions
  per the assessment's `id_regions`, stores blobs, enqueues `scan.identify` per page.
  No `id_regions` for all three kinds ⇒ the batch parks at upload time with a clear
  error ("draw the three regions first") rather than half-running.
- **`scan.identify`** — OCRs the three crops (existing VisionProvider seam and
  provider/model choice per batch), runs the matcher (§6), writes proposal +
  auto-assignment or leaves the page an orphan.
- **`scan.promote`** — per assigned page at finalize (§8).

> **Implemented as named differently** (D68): the actual River job kinds are
> `scan.split`, `scan.render_pages`, `scan.identify_page`, `scan.promote_page` —
> distinct strings from any retired file-level job kind, so a stale arg shape
> already queued at deploy time can never mis-decode into the new job body. See
> `DECISIONS.md` D68.

## 6. Matching & assignment semantics (D64, D65)

**Student rung — both-or-orphan (D64).** Auto-assign requires the ID read and the
name read to *independently* resolve to the same live (non-withdrawn) roster student:

- Student ID: normalize (trim, case-fold, strip spaces), then **exact** match against
  roster external IDs. Never fuzzy — a one-digit OCR error is exactly how a page lands
  on the wrong real student.
- Name: the existing conservative rungs (`match.go`): normalized exact match;
  ambiguity (two live students within tolerance, duplicate names) always fails to no
  match. An illegible or low-confidence name read means **no auto-assign even with a
  clean ID** — the page orphans with the ID-matched student pre-filled
  (`proposal_source = 'ocr_id'`).
- Disagreement (ID → student A, name → student B): orphan, flagged distinctly in the
  queue (possible wrong-ID-written case), no pre-fill.

**Problem rung.** Strip accepted prefixes (`Q`, `P`, `q`, `p`, `問`, `第`, `#`,
optional trailing `.`, `題`), parse the integer, validate against the assessment's
problem numbers. Unreadable or out-of-range ⇒ orphan (with student pre-filled if the
student rung passed).

**Occupied cells — never overwrite (D65).** Auto-assign fills **empty** cells only
(no live assigned page, no live submission for that cell). A page resolving to an
occupied cell parks instead:

- `image_sha256` equal to the incumbent's ⇒ `duplicate`: no action required,
  collapsed list in the UI. Re-uploading the same giant PDF is therefore a no-op
  except that it fills previously missing/discarded cells — the "at most it fills the
  missing" rule.
- different content ⇒ `conflict`: side-by-side chooser (keep incumbent / replace /
  discard new). Replace onto a **graded** cell routes through ingest's existing
  force-replace guard and is always an explicit human action; auto-assign can never
  touch graded work. Manual assignment onto an occupied cell prompts with the
  incumbent the same way.

**Orphan queue.** Everything not auto-assigned. Each entry shows the page thumbnail
plus the three crops; actions: assign to (student, problem) (pickers pre-filled from
partial proposals) or discard (blank/noise pages). Nothing in the queue blocks other
pages or finalize of assigned pages.

## 7. Identify tab (rebuilt)

1. **Region editor** — evolved `IDRegionCard`: exactly three rects, fixed colors
   (student ID blue, name green, problem orange), drawn on a sample page (uploaded
   template image or any rendered batch page). Editing regions after OCR offers
   **re-identify unresolved pages** — re-runs render-crop + identify for orphans only;
   assigned/promoted pages are never disturbed.
2. **Upload card** — PDFs and/or zip; OCR provider/model pickers; no problem-scope
   selector.
3. **Assignment matrix** — the centerpiece: rows = roster students, columns =
   problems (~250×8, virtualized). Cell states: auto-assigned / manually assigned /
   promoted / conflict / empty(missing). Row+column tallies; filters only-missing and
   only-conflicts. Cell click: page image + crops, unassign, open conflict. This is
   the assessment-wide truth view replacing per-batch reconciliation.
4. **Orphan queue** — keyboard-driven list (§6); duplicates collapsed below;
   conflicts get the side-by-side chooser.
5. **Finalize card** — assessment-wide: missing-cell count, ack-missing dialog, then
   one `scan.promote` per assigned-unpromoted page. Re-finalize is safe and
   incremental (only newly assigned pages promote).

> **Deferred** (final-review fix wave, 2026-07-04): item 1's "re-identify unresolved
> pages after region edits" was NOT implemented this round — regions are expected to
> be final before upload (consistent with D66's append-only stance: promotion seeds
> mask regions from `student_id`/`name` once, not on every region edit). Re-drawing a
> region after OCR has already run does not currently trigger any re-crop/re-identify
> pass; an operator who needs that must re-upload the affected pages as a fresh batch.
> What DOES exist today: per-page **Retry** in the orphan queue re-enqueues an
> **errored** page's next stage (identify when crops exist, else render) — that is a
> different, narrower capability (error recovery, not a region-edit-driven re-run) and
> should not be confused with the deferred item above.

## 8. Promotion & masking (D66)

`scan.promote` loads the page image and calls
`ingest.Ingest(assessmentID, IngestInput{Filename: "<external_id>.<ext>", Data: image,
Kind: "image", TargetProblemID: assigned_problem_id}, promotedBy, force)` — the D22
path, unchanged: per-problem supersede chain, graded/published guards, answer + page
materialization. Success stamps `submission_id`; promoted pages are skipped on
re-finalize. `force=true` flows only from the explicit conflict "replace graded"
action.

**Masking seed (D66):** every page now carries identity at known coordinates, so
promotion seeds per-page mask regions from the `student_id` and `name` id_regions
(not `problem_id` — the grader may use it), feeding the existing `mask.page` job.
Identity is masked on every page before AI grading, not just where mask regions were
hand-drawn.

## 9. Deletions & migration (D67)

Deleted with the old flow: `scan_files` (table, queries, service/HTTP paths), the
assessment-wide one-file-per-student conflict, positional page→problem mapping within
scan promotion, per-problem batch scoping, the ReviewStrip serial-confirm UI,
per-batch reconciliation. The migration drops `scan_files` outright; any batch still
staged at upgrade time is lost — finalize or discard in-flight batches before
migrating (solo deployment; called out in the migration header).

## 10. Errors & privacy

Per-page `error` + `attempts` with River retries (render failure, OCR outage);
errored pages surface in batch progress with re-run. Blank/noise pages arrive as
orphans and get discarded. Split idempotent via `(batch_id, page_index)`; identify
idempotent per page (re-runs overwrite OCR/proposal only while unassigned). OCR text
and crops are student PII: blobs only, never logged, existing convention.

## 11. Testing (test-first)

- **Unit:** problem-ID parser (prefix vocabulary incl. CJK, out-of-range, garbage);
  agreement matcher (agree / disagree / ambiguous name / clean-ID-illegible-name →
  orphan with pre-fill); occupied-cell transitions (fill / duplicate / conflict /
  graded-cell guard); split chunking + idempotency; promote idempotency.
- **Store:** the live-cell partial unique index; derived status ordering.
- **HTTP integration** (existing fake renderer + fake OCR from `scans_test.go`):
  upload→split→render→identify happy path; same-PDF re-upload ⇒ all duplicates +
  fills; conflict resolution; incremental finalize; regions-missing parks the batch.

## 12. Out of scope

Multi-page answers per problem (explicitly ruled out — one page per problem),
automatic de-skew/rotation correction, cross-assessment batches, changes to the
Submissions tab direct path, retention/erasure of staged scans (PLAN_GAPS B-H7).
