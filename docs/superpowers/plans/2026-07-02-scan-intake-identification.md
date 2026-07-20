# Scan Intake & Student Identification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Randomly-named exam scans (zip / PDFs / single-page images) get ID-region-cropped, OCR-proposed against the roster, human-confirmed one by one, and promoted through the existing ingestion guards — per spec `docs/superpowers/specs/2026-07-02-scan-intake-identification-design.md` (D18–D23).

**Architecture:** Staging tables (`scan_batches`/`scan_files`) + three River jobs (`scan.expand`, `scan.render`, `scan.identify`) in front of the existing ingest tail; a sealed `imaging.ProviderImage` interface admits exactly `MaskedImage` (grading) and `IDCrop` (identification) to the provider layer; promotion at finalize reuses `ingest` guards unchanged.

**Tech Stack:** existing stack only — Go 1.25+, pgx/sqlc/goose, River, go-pdfium, stdlib image + archive/zip, React/TS/Tailwind/react-query.

## Global Constraints

- Everything in the overnight plan's Global Constraints still applies (uv-only Python, PII logging ban D14, seams-only deps, test-first, no push, decimal strings for points).
- New goose migration is `0010_scan_intake.sql` and **must have a working Down**.
- OCR text (student IDs / names read by the model) may live in `scan_files` columns but **never** in slog, job args, error strings, or audit detail beyond row IDs.
- A provider request carries **identity XOR answer content** (D19): no code path may send an unmasked full page; absent id_regions ⇒ no OCR at all.
- Unit tests offline (fake renderer, local-disk blobstore, `llm.StaticSource` + scripted fake); DB-touching tests use the existing `storetest` harness and skip without `ADAMARKER_TEST_DATABASE_URL`.
- Frontend mutations only via `lib/api.ts` (CSRF header); after finalize/reassign invalidate `["ingest-report", assessmentId]` + `["problem-summaries", assessmentId]`.

---

## Milestone S1 — schema + seam widening (no behavior change yet)

### Task 1: Migration 0010 + sqlc regen + rename fallout

**Files:** Create `migrations/0010_scan_intake.sql`; Modify `internal/store/queries/*.sql` (rename touchpoints), regen via `make sqlc`; fix compile fallout in `internal/ingest/ingest.go`, `internal/httpapi/ingestion.go`, `internal/httpapi/review.go` (`PdfRef→SourceRef`, `PdfSha256→SourceSha256`).

Schema exactly per spec §3:
- `submissions`: RENAME `pdf_ref→source_ref`, `pdf_sha256→source_sha256`; ADD `source_kind TEXT NOT NULL DEFAULT 'pdf' CHECK (source_kind IN ('pdf','image'))`, `problem_id BIGINT NULL`, composite FK `(problem_id, assessment_id) REFERENCES problems (id, assessment_id)`, `retracted_at TIMESTAMPTZ`; DROP index `submissions_active_uniq`, create the two partial indexes (whole-assessment / per-problem, both `WHERE superseded_by IS NULL AND retracted_at IS NULL`).
- `students`: ADD `withdrawn_at TIMESTAMPTZ`.
- CREATE `scan_batches`, `scan_files` (all columns from spec §3, `UNIQUE (batch_id, source_sha256)`), `id_regions` (mask_regions shape + `page_index INT NOT NULL DEFAULT 0`).
- Down: drop new tables, drop new columns/indexes, restore renames + old index.

Steps: write migration → `make sqlc` → fix builds → run existing tests (`make test`) green → migration up/down integration test passes (existing harness runs downs) → commit.

### Task 2: `imaging.Crop` + `IDCrop` + sealed `ProviderImage`

**Files:** Modify `internal/imaging/mask.go` (add sealed interface + implement on `MaskedImage`), Create `internal/imaging/crop.go` + `internal/imaging/crop_test.go`; Modify `internal/llm/llm.go` (`Request.Images []imaging.ProviderImage`), `internal/grading/runner.go` (slice conversion), adapters' tests if they construct Requests.

**Produces (later tasks consume):**
```go
type ProviderImage interface { JPEG() []byte; SHA256() string; sealedProviderImage() }
func Crop(originalJPEG []byte, regions []Region, quality int) (IDCrop, error) // rects cropped individually, stacked vertically, JPEG q85 default
func LoadIDCrop(key string, jpegBytes []byte) (IDCrop, error)                 // requires "/idcrop/" key segment
```

Tests (write first): crop extracts the right pixels (paint a known-color rect, crop it, assert dominant color + dims incl. padding); multiple regions stack (height = Σ heights, width = max); out-of-bounds region clamped, fully-outside region error (a crop of nothing is a bug, unlike masking); `LoadIDCrop` rejects non-`/idcrop/` keys; `LoadMasked` still rejects `/idcrop/` keys; `MaskedImage` and `IDCrop` both satisfy `ProviderImage`; compile-fail guard comment (can't test un-implementable interface, document the seal). Then `make test` green (runner conversion `[]MaskedImage → []ProviderImage` loop) → commit.

### Task 3: ingest widening — `IngestInput`, images, scoping, retraction, withdrawn

**Files:** Modify `internal/ingest/ingest.go` + `ingest_test.go`, `internal/store/queries/ingestion.sql` + `students.sql`, `internal/httpapi/ingestion.go` (callers), `internal/httpapi/students.go`.

**Produces:**
```go
type IngestInput struct {
    Filename        string // still carries "<student_id>.<ext>" for roster match
    Data            []byte
    Kind            string // "pdf" | "image"
    TargetProblemID int64  // 0 = whole-assessment positional mapping
}
func (s *Service) Ingest(ctx context.Context, assessmentID int64, in IngestInput, uploadedBy int64, force bool) FileResult
func (s *Service) RetractSubmission(ctx context.Context, submissionID int64, actor int64, force bool) error
func NormalizeImage(data []byte, opts render.Options) (render.Page, error) // decode png/jpeg → downscale MaxLongEdgePx → JPEG q85
```

Behavior (test-first, extending `ingest_test.go` patterns):
- `IngestFile` becomes a thin wrapper over `Ingest` (existing callers unchanged in behavior; existing tests must stay green).
- `Kind=="image"`: `NormalizeImage` synthesizes the single rendered page; `page_count=1`; original bytes stored as `source_ref` with proper extension; `source_kind='image'`.
- `TargetProblemID != 0`: all pages map to that problem (ordered page_index), submission row carries `problem_id`; supersede/graded/published guards scope to that problem (new queries `CountRecordsForStudentProblem`, `CountPublishedForStudentProblem`, `GetActiveSubmissionForProblem`); page deletion by submission (`DeletePagesBySubmission` replaces `DeletePagesForStudentAssessment` — also used by the whole-assessment path, equivalent there).
- `RetractSubmission`: sets `retracted_at`, deletes its pages; blocked without force if student+scope has records; blocked always if published.
- Withdrawn: `MaterializeAnswers` gains `WHERE withdrawn_at IS NULL`; `IngestReportRows` excludes withdrawn from expected; `PATCH /api/students/{id}` `{withdrawn: bool}` (lecturer+, audit-logged) + `withdrawn` field in student JSON.
- `handleSubmissionPDF` streams by `source_kind` (Content-Type image/jpeg|png vs application/pdf).

`make test` green → commit (this is the largest core task; commit sub-slices: images, scoping, retraction, withdrawn).

---

## Milestone S2 — staging backend

### Task 4: roster matcher (pure Go)

**Files:** Create `internal/scan/match.go`, `internal/scan/match_test.go`.

**Produces:**
```go
type RosterEntry struct { ID int64; ExternalID, Name string } // withdrawn already filtered out by caller
type Proposal struct { StudentID int64; Source string } // Source: filename|ocr_id|ocr_fuzzy|ocr_name; zero Proposal = unidentified
func Match(filename, ocrID, ocrName string, roster []RosterEntry) (Proposal, bool /*conflict: filename vs OCR disagree*/)
func NormalizeID(s string) string   // NFKC fold, uppercase, strip non-alphanumerics
func NormalizeName(s string) string // NFKC, strip all whitespace, casefold
```

Table tests first: full-width `ｂ１０９０２０６６` → `B10902066`; CJK name with spaces `王 小 明` matches `王小明`; ladder order incl. filename stem; fuzzy = unique Levenshtein≤1 **and** name confirms; ambiguous fuzzy → no proposal; unique-name match; filename/OCR conflict flag; empty roster. Implement (tiny Levenshtein, `golang.org/x/text/unicode/norm` for NFKC — already an indirect dep of the module tree; if not, hand-roll width folding for ASCII fullwidth range + keep NFC via norm). `make test` → commit.

### Task 5: `scan.Service` — batches, files, assignment, finalize

**Files:** Create `internal/scan/scan.go`, `internal/scan/scan_test.go` (storetest + fake renderer + local blobstore + `llm.StaticSource`), `internal/store/queries/scan.sql` (+regen).

**Produces (queue + httpapi consume):**
```go
type Service struct {
    Store *store.Store; Blobs blobstore.Store; Renderer render.Renderer; Opts render.Options
    Providers llm.ProviderSource; Ingest *ingest.Service; Log *slog.Logger
    EnqueueRender  func(ctx context.Context, tx pgx.Tx, fileIDs []int64) error   // injected by queue
    EnqueueIdentify func(ctx context.Context, tx pgx.Tx, fileIDs []int64) error
}
type NewBatch struct { ProblemID int64; OCREnabled bool; OCRProvider, OCRModel string }
func (s *Service) CreateBatch(ctx, assessmentID int64, nb NewBatch, files []Upload, zipData []byte, actor int64) (BatchView, error)
func (s *Service) Expand(ctx, batchID int64) error        // scan.expand worker body
func (s *Service) RenderFile(ctx, fileID int64) error     // scan.render worker body
func (s *Service) IdentifyFile(ctx, fileID int64, finalAttempt bool) error // scan.identify worker body
func (s *Service) Assign(ctx, fileID, studentID, actor int64) error       // 409-style ErrConflict when student already has an assigned file in this assessment's open batches or an active submission
func (s *Service) Unassign / Discard / Undiscard / Reassign / Retry(...)
func (s *Service) Finalize(ctx, batchID int64, ackMissing, force bool, actor int64) (FinalizeReport, error)
```

Behavior (test-first):
- `CreateBatch`: batch row + either scan_file rows (loose files; bytes → blob `assessments/{aid}/scans/{batch}/{sha16}.<ext>`; dedupe on `UNIQUE(batch_id, source_sha256)` → skipped-duplicates reported) + tx-enqueue renders, or `zip_ref` + tx-enqueue expand. Reject >50 MiB entries, >1 GiB zip, unknown extensions (report, don't fail batch).
- `Expand`: `archive/zip` over stored bytes; skip `__MACOSX/`, dotfiles, dirs; create files + enqueue renders; idempotent via the sha unique (re-delivery safe).
- `RenderFile`: kind-dispatch (PDF `PageCount`+`RenderPage(p)` with p = id_regions page_index clamped; image `ingest.NormalizeImage`); store `page0_image_ref` + dims; if id_regions exist → `imaging.Crop` → store under `assessments/{aid}/scans/{batch}/idcrop/{sha8}.jpg` + enqueue identify when `ocr_enabled`; no regions ⇒ no crop, no identify (D19). Decode failure → `error` column, terminal.
- `IdentifyFile`: resolve provider via `ProviderSource` (limiter.Wait), `Grade(ctx, model, llm.Request{System: idSystemPrompt, Prompt: idUserPrompt, Images: []imaging.ProviderImage{crop via LoadIDCrop}, Schema: idSchema, Temperature: 0, MaxTokens: 256, ToolName: "submit_identity"})` — add optional `ToolName` field to `llm.Request` (empty ⇒ existing `submit_grade`) in both adapters (+adapter tests). Parse strict `{student_id, name, legible}`; one re-ask on malformed; then `Match` against non-withdrawn roster → write ocr_* + proposal columns. Rate-limit errors retryable; `ProviderUnavailableError`/final attempt → `error` column (copy `gradeLeaf` taxonomy).
- `Assign`: conflict check (another file assigned to that student in this assessment's batches, or active whole/scope submission not from reassignment) → typed `ErrConflict{OtherFileID}`; sets assigned_*.
- `Finalize`: every file terminal or error → 409 detail; missing = active non-withdrawn students with no assigned file across this assessment's batches and no active submission → require ackMissing (audit-log the acknowledged IDs count + batch id, not names); promote each assigned unpromoted file via `Ingest(IngestInput{Filename: extID+ext, Data, Kind, TargetProblemID: batch.ProblemID}, actor, force)`; per-file failure recorded in `error`, keeps batch open; all promoted ⇒ `finalized_at`. Idempotent re-run skips `submission_id IS NOT NULL`.
- `Reassign` (works pre/post-promotion): if promoted → `ingest.RetractSubmission(old)` + `Ingest` for the new student (same force/published guards) in sequence; else just re-point `assigned_student_id`.

Scripted fake provider: extend `internal/llm/fake` with a `Script []Step`-style constructor (or a `StaticJSON` provider) so identify tests don't depend on the rubric-shaped fabricator. Commit per slice (create/expand, render, identify, assign/finalize).

### Task 6: River jobs + queue wiring

**Files:** Modify `internal/queue/river.go` (+`river_test.go` if present pattern), `cmd/adamarker/main.go`.

- Args (IDs only): `ScanExpandArgs{BatchID}` kind `scan.expand`; `ScanRenderArgs{FileID}` kind `scan.render`; `ScanIdentifyArgs{FileID}` kind `scan.identify`.
- Queues: add `"scan": {MaxWorkers: 2}`; identify jobs go on existing `"llm"` queue, MaxAttempts 3, `Timeout` 5 min (copy leafWorker).
- `queue.New(pool, runner, logger)` → `queue.New(pool, Deps{Runner *grading.Runner, Scans *scan.Service}, logger)`; inject `Scans.EnqueueRender/EnqueueIdentify = c.enqueue...Tx` (mirror `runner.EnqueueLeaves`); main.go builds `scan.Service` before `queue.New`.
- Workers call the Task-5 bodies; `IdentifyFile(ctx, id, job.Attempt >= 3)`.

`make test` + existing api tests green → commit.

### Task 7: HTTP API

**Files:** Create `internal/httpapi/scans.go` + `internal/httpapi/scans_test.go` (phase-test pattern); Modify `internal/httpapi/api.go` (routes), `internal/httpapi/students.go` (PATCH), `frontend`-facing JSON kept decimal-string-free (geometry floats fine).

Routes per spec §10 (all TA+ unless noted):
```
POST /api/assessments/{id}/scan-batches      multipart: files[] | zip; fields problem_id?, ocr_enabled?, ocr_provider?, ocr_model?
GET  /api/assessments/{id}/scan-batches
GET  /api/scan-batches/{id}                  files + reconciliation {missing[], withdrawn[], conflicts[]}
POST /api/scan-batches/{id}/finalize         {ack_missing?, force?}
POST /api/scan-files/{id}/assign             {student_id}  (external ID)
POST /api/scan-files/{id}/unassign | discard {reason} | undiscard | reassign {student_id, force?} | retry
GET  /api/scan-files/{id}/crop | page        image/jpeg streams (streamBlob)
GET|PUT /api/assessments/{id}/id-regions     mask-regions handler pattern + shared page_index validation
PATCH /api/students/{id}                     {withdrawn: bool} (lecturer+)
```
Derived per-file `state` in JSON: `error|discarded|promoted|assigned|proposed|unidentified|processing`. Handler tests: upload loose files (fake renderer) → files listed → assign → conflict 409 → finalize (ack_missing) → ingest-report reflects promotion; id-regions validation (mixed page_index → 400). Commit.

---

## Milestone S3 — frontend + polish

### Task 8: RectEditor extraction + Identify tab (regions + upload)

**Files:** Create `frontend/src/components/RectEditor.tsx` (extract MaskingTab's RegionEditor drag machinery: create/move/resize, normalized coords, generic `{rects, onChange, imageUrl}` props); Modify `frontend/src/pages/MaskingTab.tsx` to consume it (no behavior change); Create `frontend/src/pages/IdentifyTab.tsx` (ID-region card with copy-to-mask button; upload card: drop zip/files, batch options incl. provider/model selects seeded from `GET /api/providers`, OCR toggle; batch list with poll-derived progress); Modify `frontend/src/pages/AssessmentDetail.tsx` (TABS + branch), `frontend/src/lib/types.ts`.

Verify with `npm run build` (via `make frontend`) + manual dev-server pass; commit (two slices: extraction refactor, then tab).

### Task 9: assignment review strip + reconciliation/finalize

**Files:** Modify `frontend/src/pages/IdentifyTab.tsx` (+small components in-file per house style).

MaskReviewPanel pattern: keyboard `j/k` navigate, `Enter` confirm proposal, `e` roster search (combobox over `GET /api/students`, excludes withdrawn), `d` discard, `v` toggle crop↔full page (SafeImage on `/crop` and `/page`), pending-only filter, dup/conflict badges, `queryClient.setQueryData` cursor-stable patching. Reconciliation card: missing (with ack flow in finalize dialog), withdrawn grayed list, conflicts; finalize mutation → invalidate scan + ingest-report + problem-summaries keys. Commit.

### Task 10: students withdrawn toggle + docs + full verify

**Files:** Modify `frontend/src/pages/Students.tsx` (withdrawn badge + toggle, lecturer-gated), `docs/PLAN_GAPS.md` (annotate B-H8/B-H17/§13 submission-format as addressed by D18–D23), `README.md` if it lists features.

Full verify: `make test`, `make build`, `make frontend`; run the code-review skill over the whole diff; fix findings; final commit.

---

## Self-review notes

- Spec §5 "no regions ⇒ no OCR" is enforced in Task 5 RenderFile (not identify) — identify is only ever enqueued with a crop present.
- `llm.Request.ToolName` addition (Task 5) touches both adapters — their unit tests already pin the tool-name JSON; update them in the same commit.
- Task 3's `DeletePagesBySubmission` replaces the old query everywhere — check `retry`/re-upload path tests still pass.
- Withdrawn exclusion appears in four places (materialize, report, matcher input, missing-list) — each has a test.
