# Page-Level Scan Intake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the file-level scan staging pipeline (one file = one student's whole paper) with a page-native pipeline: split giant scanner PDFs into per-page rows, OCR three fixed header regions (student ID / name / problem ID) on every page, auto-assign only on ID+name agreement, park duplicates/conflicts without ever overwriting, review orphans in a queue, and promote each assigned page directly into a per-problem image submission.

**Architecture:** Spec: `docs/superpowers/specs/2026-07-04-page-level-scan-intake-design.md`. The staging unit becomes a `scan_pages` row (one per physical page) belonging to a `scan_sources` row (one per uploaded PDF/image; needed because a batch may carry several source files — a refinement of the spec's `UNIQUE(batch_id, page_index)`, which assumed one source per batch). River jobs `scan.expand → scan.split → scan.render_pages (chunked) → scan.identify_page` populate and identify pages; assessment-wide finalize enqueues `scan.promote_page` per assigned page, each calling the existing `ingest.Ingest(Kind:"image", TargetProblemID:…)` seam unchanged. The old `scan_files` flow (positional page→problem mapping, one-file-per-student 409, ReviewStrip serial confirm) is deleted, not kept alongside.

**Tech Stack:** Go 1.26 (stdlib-first), Postgres + sqlc (pgx/v5), River queue, PDFium-WASM renderer seam, React 19 + TanStack Query + Tailwind (no frontend test runner — the gate is `npm run typecheck`).

## Global Constraints

- **Test-first** for all new Go logic (CLAUDE.md): write the failing test, watch it fail, implement, watch it pass, commit.
- `make test` = unit tests (no Postgres). `make test-integration` = starts the docker test DB and runs everything with `ADAMARKER_TEST_DATABASE_URL` set; store/httpapi/scan service tests skip themselves without it. Run single packages with e.g. `ADAMARKER_TEST_DATABASE_URL="postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable" go test ./internal/scan -run TestX -v` (after `make db-test-up`).
- **Never log, commit, or paste student PII** — OCR text columns are PII: select them only into DB-bound rows or staff-facing JSON, never into logs, job args, or error strings (D14/D19 discipline).
- Job args carry **IDs only** (D14). Every worker wraps its service call in `w.client.snoozeOnShutdown(...)` (F17).
- Blob `Put` never happens inside an open DB transaction (F15). `Renderer.Open` once per file/chunk, `doc.Close()` before DB work (F3).
- The three ID-crop blobs are the only images a vision provider may see; their keys MUST contain `/idcrop/` (the `imaging.LoadIDCrop` gate rejects otherwise).
- Derived status is **never stored** (D2): compute state precedence in Go/SQL from the row's fields.
- New migration number is **0029** (current highest: 0028). Migrations run in-process via goose at startup; `make sqlc` (= `go tool sqlc generate`) regenerates `internal/store/db` after any query/migration change.
- Spec simplification (conscious YAGNI deviations, approved shape unchanged): the 2 GiB source cap is a package-level `var MaxSourceBytes int64` overridable in tests, not a config key; multi-source batches use `scan_sources` + `UNIQUE(source_id, page_index)` instead of the spec's single-source `UNIQUE(batch_id, page_index)`; `scan_batches.finalized_at` is dropped (finalize is assessment-wide and incremental — batch-level completion no longer means anything).
- In-flight `scan_files` batches are dropped by migration 0029 — finalize or discard them before deploying (solo deployment; called out in the migration header).

---

### Task 1: Cut over the schema and tear down the file-level flow

Everything compiles and `make test` passes at the end of this task; the Identify feature is temporarily a stub between Task 1 and Task 13.

**Files:**
- Create: `migrations/0029_page_level_scan_intake.sql`
- Rewrite: `internal/store/queries/scan.sql`
- Rewrite: `internal/scan/scan.go` → keep only a slim `internal/scan/service.go` (delete `scan.go` after extracting)
- Delete: `internal/scan/scan_test.go`
- Modify: `internal/scan/localocr.go` (unchanged content, stays), `internal/scan/match.go` (delete `Match`, `matchFilename`, `matchOCRFuzzy`, `ConflictDerived` uses; keep `RosterEntry`, `Proposal` deleted too — see step 6), `internal/scan/match_test.go` (trim to kept functions)
- Rewrite: `internal/httpapi/scans.go` (only id-regions handlers survive, kind-aware)
- Delete: `internal/httpapi/scans_test.go` (recreated in Task 10)
- Modify: `internal/httpapi/api.go:187-201` (scan route block)
- Modify: `internal/queue/river.go` (delete scan render/identify/promote args+workers+helpers; keep expand), `internal/queue/river_test.go`
- Modify: `cmd/adamarker/main.go` (scan service construction — field list shrinks)
- Modify: `frontend/src/lib/types.ts` (delete old scan types, keep `IDRegion` reshaped), `frontend/src/pages/IdentifyTab.tsx` (stub), delete `frontend/src/components/identify/UploadCard.tsx`, `BatchListCard.tsx`, `ReviewStrip.tsx`, `ReconciliationCard.tsx` (keep `IDRegionCard.tsx` + `useSamplePage.ts`, updated in Task 11)

**Interfaces:**
- Consumes: migration/`sqlc` conventions (see Global Constraints).
- Produces: tables `scan_sources`, `scan_pages`; reshaped `id_regions` (adds `kind`, drops `page_index`); `scan_batches` minus `problem_id`/`finalized_at`, `zip_ref` renamed `source_ref`; generated types `db.ScanSource`, `db.ScanPage`, `db.IDRegion{…, Kind string}`; slim `scan.Service` struct (fields: `Store, Blobs, Renderer, Opts, Providers, Ingest, Log, Local` + enqueue seams `EnqueueExpand func(ctx, tx, batchID int64) error`, `EnqueueSplit func(ctx, tx, sourceIDs []int64) error`, `EnqueueRenderPages func(ctx, tx, sourceID int64, pageIDs []int64) error`, `EnqueueIdentifyPages func(ctx, tx, pageIDs []int64) error`, `EnqueuePromotePages func(ctx, tx, items []PromotePage) error`); `type PromotePage struct{ PageID int64; Force bool; Actor int64 }`; kept helpers `readAll`, `int8OrNull`, `textOrNull`, `int4Of`, `boolOf`, `itoa`, `nz`, `isInterruption`, `retryableError`, `acceptedExt`, `baseName`, `openZip`, `acceptZipEntry`, `readZipEntry`; kept match helpers `RosterEntry`, `matchOCRID`, `matchOCRName`, `NormalizeID`, `NormalizeName`, `levenshtein`; constants `MaxEntryBytes`, `MaxZipBytes`, `cropQuality`, new `var MaxSourceBytes int64 = 2 << 30`, `const renderChunkSize = 25`.

- [ ] **Step 1: Write migration 0029**

Create `migrations/0029_page_level_scan_intake.sql`:

```sql
-- +goose Up
-- Page-level scan intake (design spec 2026-07-04): the staging unit becomes one
-- physical PAGE, assigned to a (student, problem) cell. Replaces the file-level
-- scan_files flow (one file = one student's whole paper, positional page→problem
-- mapping). DEPLOY NOTE: any batch still staged in scan_files is dropped here —
-- finalize or discard in-flight batches before upgrading.

DROP TABLE scan_files;

-- A batch is just "one uploaded scanner run": per-problem scoping is obsolete
-- (every page names its own problem) and batch-level finalized_at is meaningless
-- (finalize is assessment-wide and incremental). source_ref holds the zip blob
-- when the batch was a zip upload.
ALTER TABLE scan_batches DROP COLUMN problem_id;
ALTER TABLE scan_batches DROP COLUMN finalized_at;
ALTER TABLE scan_batches RENAME COLUMN zip_ref TO source_ref;

-- id_regions become typed: exactly one live region per kind, applied to EVERY
-- page (the old single-identity-page page_index is gone). Existing rows carry no
-- kind, so they are dropped — regions must be redrawn once after this upgrade.
DELETE FROM id_regions;
ALTER TABLE id_regions DROP COLUMN page_index;
ALTER TABLE id_regions
    ADD COLUMN kind TEXT NOT NULL CHECK (kind IN ('student_id', 'name', 'problem_id'));

-- One uploaded source file (a scanner PDF or a loose image). A batch may carry
-- several sources (several PDFs, or a zip's entries), so page idempotency is
-- per-source, not per-batch.
CREATE TABLE scan_sources (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (source_kind IN ('pdf', 'image')),
    page_count INT,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, source_sha256)
);
CREATE INDEX scan_sources_batch_idx ON scan_sources (batch_id);

-- One physical page. OCR text columns are PII (D14): DB-bound rows and
-- staff-facing JSON only, never logs or job args. Status is derived, never
-- stored (D2): error → discarded → promoted → parked → assigned → orphan
-- (identified_at set, no assignment) → processing.
CREATE TABLE scan_pages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id BIGINT NOT NULL REFERENCES scan_sources (id) ON DELETE CASCADE,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    page_index INT NOT NULL,
    image_ref TEXT,
    image_sha256 TEXT,
    image_width INT,
    image_height INT,
    student_id_crop_ref TEXT,
    name_crop_ref TEXT,
    problem_crop_ref TEXT,
    ocr_student_id TEXT,
    ocr_name TEXT,
    ocr_problem TEXT,
    ocr_student_id_legible BOOLEAN,
    ocr_name_legible BOOLEAN,
    ocr_problem_legible BOOLEAN,
    ocr_engine TEXT,
    identified_at TIMESTAMPTZ,
    proposed_student_id BIGINT REFERENCES students (id),
    proposed_problem_id BIGINT,
    proposal_source TEXT CHECK (proposal_source IN ('ocr_agree', 'ocr_id', 'ocr_name', 'ocr_disagree')),
    assigned_student_id BIGINT REFERENCES students (id),
    assigned_problem_id BIGINT,
    assigned_by BIGINT REFERENCES users (id),
    assigned_at TIMESTAMPTZ,
    force_promote BOOLEAN NOT NULL DEFAULT FALSE,
    parked_reason TEXT CHECK (parked_reason IN ('duplicate', 'conflict')),
    parked_against BIGINT REFERENCES scan_pages (id),
    discarded_at TIMESTAMPTZ,
    discard_reason TEXT,
    submission_id BIGINT REFERENCES submissions (id),
    error TEXT,
    attempts INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- split idempotency under River redelivery
    UNIQUE (source_id, page_index),
    -- assignment is to a CELL: both set or both null
    CHECK ((assigned_student_id IS NULL) = (assigned_problem_id IS NULL)),
    -- a proposed/assigned problem must belong to this page's assessment
    -- (mirrors the answers pattern in 0003)
    FOREIGN KEY (proposed_problem_id, assessment_id) REFERENCES problems (id, assessment_id),
    FOREIGN KEY (assigned_problem_id, assessment_id) REFERENCES problems (id, assessment_id)
);
CREATE INDEX scan_pages_batch_idx ON scan_pages (batch_id);
CREATE INDEX scan_pages_assessment_idx ON scan_pages (assessment_id);
CREATE INDEX scan_pages_source_idx ON scan_pages (source_id);
-- One live assigned page per (assessment, student, problem) cell — mirrors
-- submissions_active_problem_uniq.
CREATE UNIQUE INDEX scan_pages_live_cell_uniq
    ON scan_pages (assessment_id, assigned_student_id, assigned_problem_id)
    WHERE assigned_student_id IS NOT NULL AND discarded_at IS NULL;

-- +goose Down
DROP TABLE scan_pages;
DROP TABLE scan_sources;

ALTER TABLE id_regions DROP COLUMN kind;
ALTER TABLE id_regions ADD COLUMN page_index INT NOT NULL DEFAULT 0;

ALTER TABLE scan_batches RENAME COLUMN source_ref TO zip_ref;
ALTER TABLE scan_batches ADD COLUMN finalized_at TIMESTAMPTZ;
ALTER TABLE scan_batches ADD COLUMN problem_id BIGINT NULL;
ALTER TABLE scan_batches
    ADD CONSTRAINT scan_batches_problem_assessment_fkey
        FOREIGN KEY (problem_id, assessment_id) REFERENCES problems (id, assessment_id);

-- scan_files restored per 0010 + ocr_engine from 0011.
CREATE TABLE scan_files (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES scan_batches (id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (source_kind IN ('pdf', 'image')),
    page_count INT,
    page0_image_ref TEXT,
    page0_width INT,
    page0_height INT,
    id_crop_ref TEXT,
    ocr_student_id TEXT,
    ocr_name TEXT,
    ocr_legible BOOLEAN,
    proposed_student_id BIGINT REFERENCES students (id),
    proposal_source TEXT CHECK (proposal_source IN ('filename', 'ocr_id', 'ocr_fuzzy', 'ocr_name')),
    assigned_student_id BIGINT REFERENCES students (id),
    assigned_by BIGINT REFERENCES users (id),
    assigned_at TIMESTAMPTZ,
    discarded_at TIMESTAMPTZ,
    discard_reason TEXT,
    submission_id BIGINT REFERENCES submissions (id),
    error TEXT,
    attempts INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ocr_engine TEXT,
    UNIQUE (batch_id, source_sha256)
);
CREATE INDEX scan_files_batch_idx ON scan_files (batch_id);
```

- [ ] **Step 2: Rewrite `internal/store/queries/scan.sql`**

Replace the whole file with:

```sql
-- Page-level scan intake queries (design spec 2026-07-04). All OCR text columns
-- hold PII (D14): callers select them only into DB-bound rows or staff-facing
-- JSON, never into logs, job args, or error strings.

-- ---- batches ----

-- name: CreateScanBatch :one
INSERT INTO scan_batches (assessment_id, ocr_enabled, ocr_provider, ocr_model, source_ref, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetScanBatch :one
SELECT * FROM scan_batches WHERE id = $1;

-- name: ListScanBatches :many
SELECT * FROM scan_batches WHERE assessment_id = $1 ORDER BY id DESC;

-- name: SetBatchSourceRef :exec
UPDATE scan_batches SET source_ref = $2 WHERE id = $1;

-- ---- sources ----

-- CreateScanSource is idempotent within a batch: UNIQUE(batch_id, source_sha256)
-- collapses duplicate uploads; ON CONFLICT DO NOTHING returns no row for a
-- duplicate (sqlc :one surfaces pgx.ErrNoRows) — caller treats as "skipped".
-- name: CreateScanSource :one
INSERT INTO scan_sources (batch_id, original_filename, source_ref, source_sha256, source_kind)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (batch_id, source_sha256) DO NOTHING
RETURNING *;

-- name: GetScanSource :one
SELECT * FROM scan_sources WHERE id = $1;

-- name: ListScanSourcesForBatch :many
SELECT * FROM scan_sources WHERE batch_id = $1 ORDER BY id;

-- name: SetScanSourcePageCount :exec
UPDATE scan_sources SET page_count = $2, error = NULL WHERE id = $1;

-- name: SetScanSourceError :exec
UPDATE scan_sources SET error = $2 WHERE id = $1;

-- ---- pages: creation & pipeline writes ----

-- CreateScanPage is idempotent per source page (UNIQUE(source_id, page_index)):
-- a redelivered scan.split re-inserts harmlessly.
-- name: CreateScanPage :one
INSERT INTO scan_pages (source_id, batch_id, assessment_id, page_index)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source_id, page_index) DO NOTHING
RETURNING *;

-- name: GetScanPage :one
SELECT * FROM scan_pages WHERE id = $1;

-- name: ListScanPagesForSource :many
SELECT * FROM scan_pages WHERE source_id = $1 ORDER BY page_index;

-- name: SetScanPageRendered :exec
UPDATE scan_pages
SET image_ref = $2, image_sha256 = $3, image_width = $4, image_height = $5,
    student_id_crop_ref = $6, name_crop_ref = $7, problem_crop_ref = $8,
    error = NULL, updated_at = now()
WHERE id = $1;

-- SetScanPageIdentified records the OCR reads + proposal and stamps
-- identified_at — the orphan/processing boundary for derived state.
-- name: SetScanPageIdentified :exec
UPDATE scan_pages
SET ocr_student_id = $2, ocr_name = $3, ocr_problem = $4,
    ocr_student_id_legible = $5, ocr_name_legible = $6, ocr_problem_legible = $7,
    ocr_engine = $8,
    proposed_student_id = $9, proposed_problem_id = $10, proposal_source = $11,
    identified_at = now(), error = NULL, updated_at = now()
WHERE id = $1;

-- ---- pages: assignment & lifecycle ----

-- AssignScanPage clears parked/discard/error state: assignment (auto or manual)
-- always wins over a stale park or error, and a TA who assigns by eye after an
-- OCR outage must not be blocked by a lingering error.
-- name: AssignScanPage :exec
UPDATE scan_pages
SET assigned_student_id = $2, assigned_problem_id = $3, assigned_by = $4,
    assigned_at = now(), parked_reason = NULL, parked_against = NULL,
    discarded_at = NULL, discard_reason = NULL, error = NULL, updated_at = now()
WHERE id = $1;

-- name: UnassignScanPage :exec
UPDATE scan_pages
SET assigned_student_id = NULL, assigned_problem_id = NULL, assigned_by = NULL,
    assigned_at = NULL, force_promote = FALSE, updated_at = now()
WHERE id = $1;

-- name: ParkScanPage :exec
UPDATE scan_pages
SET parked_reason = $2, parked_against = $3, updated_at = now()
WHERE id = $1;

-- name: SetScanPageForcePromote :exec
UPDATE scan_pages SET force_promote = $2, updated_at = now() WHERE id = $1;

-- name: DiscardScanPage :exec
UPDATE scan_pages
SET discarded_at = now(), discard_reason = $2, parked_reason = NULL,
    parked_against = NULL, error = NULL, updated_at = now()
WHERE id = $1;

-- name: UndiscardScanPage :exec
UPDATE scan_pages
SET discarded_at = NULL, discard_reason = NULL, updated_at = now()
WHERE id = $1;

-- name: SetScanPageError :exec
UPDATE scan_pages SET error = $2, updated_at = now() WHERE id = $1;

-- SetScanPagePromotionError stamps a promote-job failure ONLY on a
-- still-unpromoted row (same race guard as the old SetScanFilePromotionError):
-- a losing duplicate promote job must not stamp an error onto a promoted row.
-- name: SetScanPagePromotionError :exec
UPDATE scan_pages SET error = $2, updated_at = now() WHERE id = $1 AND submission_id IS NULL;

-- name: ClearScanPageError :exec
UPDATE scan_pages SET error = NULL, updated_at = now() WHERE id = $1;

-- name: SetScanPageSubmission :exec
UPDATE scan_pages SET submission_id = $2, updated_at = now() WHERE id = $1;

-- ClearPromotionErrorsForAssessment lets a Finalize re-run re-attempt pages whose
-- error came from a previous promotion ("promotion rejected: " prefix) without
-- clearing genuine render/OCR errors.
-- name: ClearPromotionErrorsForAssessment :exec
UPDATE scan_pages
SET error = NULL, updated_at = now()
WHERE assessment_id = $1
  AND assigned_student_id IS NOT NULL
  AND submission_id IS NULL
  AND discarded_at IS NULL
  AND error LIKE 'promotion rejected:%';

-- ---- cell occupancy ----

-- LivePageForCell finds the live (non-discarded) page already assigned to this
-- exact (student, problem) cell, excluding the page being placed. Promoted pages
-- count — their assignment is what the submission came from.
-- name: LivePageForCell :one
SELECT * FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id = $2 AND assigned_problem_id = $3
  AND discarded_at IS NULL AND id <> $4
ORDER BY id LIMIT 1;

-- ---- listing / progress / matrix ----

-- ScanPageRows joins proposed/assigned student externals for staff-facing JSON
-- (PII to an authenticated TA+ session only).
-- name: ScanPageRows :many
SELECT sp.*,
    ps.student_id AS proposed_external_id, ps.name AS proposed_name_roster,
    a.student_id AS assigned_external_id, a.name AS assigned_name
FROM scan_pages sp
LEFT JOIN students ps ON ps.id = sp.proposed_student_id
LEFT JOIN students a ON a.id = sp.assigned_student_id
WHERE sp.assessment_id = $1
ORDER BY sp.id;

-- ScanBatchPageProgress mirrors the derived-state precedence exactly, each
-- bucket excluding every higher-precedence condition (D2; F6: no PII columns).
-- name: ScanBatchPageProgress :many
SELECT batch_id,
    count(*) AS total,
    count(*) FILTER (WHERE error IS NOT NULL AND error <> '') AS errored,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NOT NULL
    ) AS discarded,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NOT NULL
    ) AS promoted,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NOT NULL
    ) AS parked,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NOT NULL
    ) AS assigned,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NULL
          AND identified_at IS NOT NULL
    ) AS orphaned,
    count(*) FILTER (
        WHERE NOT (error IS NOT NULL AND error <> '')
          AND discarded_at IS NULL
          AND submission_id IS NULL
          AND parked_reason IS NULL
          AND assigned_student_id IS NULL
          AND identified_at IS NULL
    ) AS processing
FROM scan_pages
WHERE batch_id = ANY (sqlc.arg(batch_ids)::bigint [])
GROUP BY batch_id;

-- name: ListLiveAssignedPagesForAssessment :many
SELECT id, assigned_student_id, assigned_problem_id, assigned_by, submission_id
FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id IS NOT NULL AND discarded_at IS NULL;

-- Live submissions for the matrix's "covered by a submission" cells (whole-
-- assessment rows cover every problem for that student).
-- name: ListLiveSubmissionsForAssessment :many
SELECT id, student_id, problem_id FROM submissions
WHERE assessment_id = $1 AND superseded_by IS NULL AND retracted_at IS NULL;

-- ---- finalize / promote ----

-- name: ListAssignedUnpromotedPages :many
SELECT * FROM scan_pages
WHERE assessment_id = $1 AND assigned_student_id IS NOT NULL
  AND discarded_at IS NULL AND submission_id IS NULL
ORDER BY id;

-- A cell is missing when an active student × problem has neither a live assigned
-- page nor a live submission covering it (per-problem, or whole-assessment).
-- Count only in the finalize gate (PII discipline).
-- name: CountMissingCells :one
SELECT count(*)
FROM students st
CROSS JOIN problems p
WHERE p.assessment_id = sqlc.arg(assessment_id)
  AND st.withdrawn_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM scan_pages sp
    WHERE sp.assessment_id = sqlc.arg(assessment_id)
      AND sp.assigned_student_id = st.id AND sp.assigned_problem_id = p.id
      AND sp.discarded_at IS NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM submissions s
    WHERE s.assessment_id = sqlc.arg(assessment_id)
      AND s.student_id = st.id
      AND s.superseded_by IS NULL AND s.retracted_at IS NULL
      AND (s.problem_id IS NULL OR s.problem_id = p.id)
  );

-- name: ListMissingCells :many
SELECT st.student_id, st.name, p.number AS problem_number
FROM students st
CROSS JOIN problems p
WHERE p.assessment_id = sqlc.arg(assessment_id)
  AND st.withdrawn_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM scan_pages sp
    WHERE sp.assessment_id = sqlc.arg(assessment_id)
      AND sp.assigned_student_id = st.id AND sp.assigned_problem_id = p.id
      AND sp.discarded_at IS NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM submissions s
    WHERE s.assessment_id = sqlc.arg(assessment_id)
      AND s.student_id = st.id
      AND s.superseded_by IS NULL AND s.retracted_at IS NULL
      AND (s.problem_id IS NULL OR s.problem_id = p.id)
  )
ORDER BY st.student_id, p.number;

-- ---- id_regions (typed: one region per kind, applied to every page) ----

-- name: ListIDRegions :many
SELECT * FROM id_regions WHERE assessment_id = $1 ORDER BY id;

-- name: DeleteIDRegions :exec
DELETE FROM id_regions WHERE assessment_id = $1;

-- name: CreateIDRegion :one
INSERT INTO id_regions (assessment_id, kind, x, y, w, h, color, padding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- ---- roster ----

-- ListActiveStudents is the matching candidate set: withdrawn students are never
-- proposed (D23).
-- name: ListActiveStudents :many
SELECT * FROM students WHERE withdrawn_at IS NULL ORDER BY student_id;

-- name: ListWithdrawnStudents :many
SELECT id, student_id, name FROM students WHERE withdrawn_at IS NOT NULL ORDER BY student_id;
```

- [ ] **Step 3: Regenerate sqlc and inspect**

Run: `make sqlc`
Expected: `internal/store/db/scan.sql.go` regenerates; `internal/store/db/models.go` now has `ScanSource`, `ScanPage` (note sqlc initialism naming: `StudentIDCropRef`, `OcrStudentID`, `OcrStudentIDLegible`, `IdentifiedAt`, `ProposedProblemID`, `ForcePromote`), `IDRegion.Kind string` (no `PageIndex`), `ScanBatch` without `ProblemID`/`FinalizedAt` and with `SourceRef`. The Go build is now broken — that's expected until step 7.

- [ ] **Step 4: Gut `internal/scan` to a slim service shell**

Delete `internal/scan/scan.go` and `internal/scan/scan_test.go`. Create `internal/scan/service.go` containing exactly: the package doc comment, the `Service` struct (Interfaces block above), `PromotePage`, the constants block

```go
const (
	// MaxEntryBytes bounds one zip entry / loose image (matches ingest's cap).
	MaxEntryBytes = ingest.MaxPDFBytes // 50 MiB
	// MaxZipBytes bounds one uploaded zip archive.
	MaxZipBytes = 2 << 30 // 2 GiB
	cropQuality = 85
	// renderChunkSize pages share one scan.render_pages job (one PDFium
	// document open per chunk instead of per page).
	renderChunkSize = 25
)

// MaxSourceBytes bounds one uploaded scanner PDF. A var so tests can shrink it;
// runtime configurability is deferred (spec §5, YAGNI).
var MaxSourceBytes int64 = 2 << 30
```

plus these helpers moved verbatim from the old scan.go: `log()`, `readAll`, `setPageError` (rename of `setFileError`, now `s.Store.Q.SetScanPageError`), `setPromotionError` (targets `SetScanPagePromotionError`), `isInterruption`, `retryableError`, `nz`, `itoa`, `int8OrNull`, `textOrNull`, `int4Of`, `boolOf`, `acceptedExt`, `baseName`, `openZip`, `acceptZipEntry`, `readZipEntry`, `storeEntryBlob`. Keep `Upload{Filename string; Data []byte}` and `SkipInfo`. Delete every other type/method (NewBatch gets redefined in Task 4 without ProblemID; BatchView, FinalizeReport, the Err* types are recreated in later tasks as specified there).

- [ ] **Step 5: Trim match.go + localocr.go tests**

In `internal/scan/match.go` delete `Match`, `matchFilename`, `matchOCR`, `matchOCRFuzzy`, `stripExt`, and the `Proposal` type (rung 3 fuzzy-ID matching and filename matching have no caller in the page flow — YAGNI). Keep `RosterEntry`, `matchOCRID`, `matchOCRName`, `NormalizeID`, `NormalizeName`, `levenshtein`. In `match_test.go` delete the tests for removed functions; keep the `NormalizeID`/`NormalizeName`/`levenshtein` and per-rung tables for `matchOCRID`/`matchOCRName` (they are package-private — tests stay in package `scan`). `localocr.go` + `localocr_test.go` are untouched.

- [ ] **Step 6: Rewrite `internal/httpapi/scans.go` to id-regions only**

Replace the file with just the id-regions handlers, kind-aware. Payload:

```go
type idRegionJSON struct {
	Kind    string  `json:"kind"`
	X       float32 `json:"x"`
	Y       float32 `json:"y"`
	W       float32 `json:"w"`
	H       float32 `json:"h"`
	Color   string  `json:"color"`
	Padding float32 `json:"padding"`
}
```

`handleGetIDRegions` is unchanged except the JSON shape. `handlePutIDRegions` keeps the normalized-0..1 validation verbatim, replaces the shared-page rule with: every `reg.Kind` must be one of `student_id|name|problem_id` (400 `"region N: kind must be student_id, name, or problem_id"`) and kinds must be distinct (400 `"region N: duplicate kind"`). Storage stays the atomic delete-then-recreate `WithTx` loop with `db.CreateIDRegionParams{AssessmentID: aid, Kind: reg.Kind, X: …, Color: color, Padding: reg.Padding}`. Delete everything else in the file (all batch/file handlers, `deriveFileState`, `scanFileJSON`, etc. — recreated for pages in Task 10). Delete `internal/httpapi/scans_test.go`.

- [ ] **Step 7: Fix the compile fallout**

- `internal/httpapi/api.go:187-201`: shrink the scan route block to only

```go
	// Scan intake (page-level, design spec 2026-07-04): id-region editor only
	// until the page endpoints land.
	api.HandleFunc("GET /api/assessments/{id}/id-regions", s.handleGetIDRegions)
	api.HandleFunc("PUT /api/assessments/{id}/id-regions", s.handlePutIDRegions)
```

- `internal/queue/river.go`: delete `ScanRenderArgs`, `ScanIdentifyArgs`, `ScanPromoteArgs`, their workers (`renderWorker`, `identifyWorker`, `promoteWorker`), `enqueueScanRenderTx`, `enqueueScanIdentifyTx`, `enqueuePromoteTx`, and the constants `scanRenderMaxAttempts`, `scanIdentifyMaxAttempts`, `scanPromoteMaxAttempts`. Keep `ScanExpandArgs`/`expandWorker`/`enqueueScanExpandTx`/`scanExpandMaxAttempts`. In `New`, the `if deps.Scans != nil` block registers only `expandWorker` and wires only `deps.Scans.EnqueueExpand = c.enqueueScanExpandTx` (the other four seams are wired in Task 9).
- `internal/queue/river_test.go`: trim `TestArgsKindsAndInsertOpts` to the surviving kinds; trim the closure-wiring test to `EnqueueExpand`.
- `cmd/adamarker/main.go`: construction is unchanged (`&scan.Service{Store: st, Blobs: blobs, Renderer: renderer, Providers: source, Ingest: ing, Log: logger}` still compiles against the slim struct).
- Other Service-literal sites (`internal/httpapi/api_test.go:83`, `internal/httpapi/regrade_test.go:100`) still compile — field names unchanged.

- [ ] **Step 8: Stub the frontend Identify tab**

- `frontend/src/lib/types.ts`: delete `ScanBatch`, `ScanBatchListRow`, `ScanFileState`, `ScanFile`, `Reconciliation`, `ScanBatchDetail`, `SkipInfo`, `CreateScanBatchResponse`, `FinalizeReport`. Reshape `IDRegion`:

```ts
/** Normalized 0..1 page coordinates; one region per kind, applied to EVERY page. */
export type IDRegionKind = "student_id" | "name" | "problem_id";

export interface IDRegion {
  kind: IDRegionKind;
  x: number;
  y: number;
  w: number;
  h: number;
  color: string;
  padding: number;
}
```

- Delete `frontend/src/components/identify/UploadCard.tsx`, `BatchListCard.tsx`, `ReviewStrip.tsx`, `ReconciliationCard.tsx`. Fix `useSamplePage.ts` by removing its scan-batch waterfall (keep only the answer-page fallback path — delete the first three queries and the `/api/scan-files/${id}/page` URL branch). Update `IDRegionCard.tsx` minimally so it compiles against the new `IDRegion` (the real 3-kind rewrite is Task 11): change `NEW_ID_REGION` to `{ kind: "student_id" as const, color: "#4a4a4a", padding: 0.01 }` and delete the `page_index` stamp in the `newRect` prop.
- `frontend/src/pages/IdentifyTab.tsx`:

```tsx
import { IDRegionCard } from "../components/identify/IDRegionCard";

export function IdentifyTab({ assessmentId }: { assessmentId: string }) {
  return (
    <div className="space-y-4">
      <IDRegionCard assessmentId={assessmentId} />
      <p className="text-sm text-neutral-500">
        Page-level scan intake is being rebuilt — upload returns in this tab shortly.
      </p>
    </div>
  );
}
```

- Delete the scan help exports that reference removed flows in `frontend/src/lib/helpContent.tsx` (`scanConflictHelp`, `scanLifecycleHelp`, `finalizeBatchHelp`, `forceRepromotionHelp`) and their usages.

- [ ] **Step 9: Verify everything is green**

Run: `go build ./... && make test`
Expected: PASS (match/localocr unit tables still green; no scan-integration tests remain).
Run: `make test-integration`
Expected: PASS — migration 0029 applies cleanly on a fresh schema (storetest recreates from zero); id-regions round-trip tests were deleted, recreated in Task 10.
Run: `cd frontend && npm run typecheck`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add -A
git commit -m "feat!: page-level scan schema; tear down file-level scan pipeline

Migration 0029: scan_files dropped, scan_sources + scan_pages created,
id_regions typed by kind, scan_batches loses problem scoping and
finalized_at. Old scan service/API/workers/frontend removed; Identify tab
stubbed until the page pipeline lands."
```

---

### Task 2: Problem-reference parser

**Files:**
- Create: `internal/scan/problemref.go`
- Test: `internal/scan/problemref_test.go`

**Interfaces:**
- Consumes: nothing (pure function; `golang.org/x/text/unicode/norm` is already a dependency via match.go).
- Produces: `func ParseProblemRef(s string) (int, bool)` — parses a handwritten problem reference (`"Q1"`, `"P3"`, `"問2"`, `"第4題"`, `"#5"`, `"3."`, `"６"` full-width) into its number. Returns `(0, false)` on anything unparseable. Validation against the assessment's real problem numbers happens at the call site (Task 6).

- [ ] **Step 1: Write the failing test**

Create `internal/scan/problemref_test.go`:

```go
package scan

import "testing"

func TestParseProblemRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"bare number", "3", 3, true},
		{"q prefix", "Q1", 1, true},
		{"lower q", "q12", 12, true},
		{"p prefix", "P4", 4, true},
		{"cjk wen prefix", "問2", 2, true},
		{"cjk di-ti wrap", "第4題", 4, true},
		{"hash prefix", "#5", 5, true},
		{"trailing dot", "3.", 3, true},
		{"trailing paren", "2)", 2, true},
		{"fullwidth digit folds", "Ｑ６", 6, true},
		{"surrounding space", " Q 7 ", 7, true},
		{"empty", "", 0, false},
		{"prefix only", "Q", 0, false},
		{"zero", "Q0", 0, false},
		{"letters after digits", "1a", 0, false},
		{"two numbers", "1 2", 0, false},
		{"name noise", "王小明", 0, false},
		{"absurdly long", "12345", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseProblemRef(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ParseProblemRef(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan -run TestParseProblemRef -v`
Expected: FAIL — `undefined: ParseProblemRef`

- [ ] **Step 3: Implement**

Create `internal/scan/problemref.go`:

```go
package scan

import (
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ParseProblemRef parses a handwritten problem reference from the problem-ID box:
// an optional prefix (Q, P, 問, 第, #), a number, and an optional suffix (., ), :,
// 題). NFKC folds full-width forms first. Anything else — including trailing
// garbage — fails: the matcher must never guess a problem (spec §6 fail-safe).
// Numbers are capped at 3 digits; an assessment with 1000+ problems is not a
// thing, and long digit runs are OCR noise.
func ParseProblemRef(s string) (int, bool) {
	folded := strings.TrimSpace(norm.NFKC.String(s))
	rs := []rune(folded)
	i := 0
	for i < len(rs) && (isProblemPrefix(rs[i]) || unicode.IsSpace(rs[i])) {
		i++
	}
	j := i
	for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
		j++
	}
	if j == i || j-i > 3 {
		return 0, false
	}
	k := j
	for k < len(rs) && (isProblemSuffix(rs[k]) || unicode.IsSpace(rs[k])) {
		k++
	}
	if k != len(rs) {
		return 0, false
	}
	n, err := strconv.Atoi(string(rs[i:j]))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func isProblemPrefix(r rune) bool {
	switch r {
	case 'Q', 'q', 'P', 'p', '問', '第', '#':
		return true
	}
	return false
}

func isProblemSuffix(r rune) bool {
	switch r {
	case '.', ')', ':', '題':
		return true
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan -run TestParseProblemRef -v`
Expected: PASS (all subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/scan/problemref.go internal/scan/problemref_test.go
git commit -m "feat(scan): handwritten problem-reference parser"
```

---

### Task 3: Strict student matcher (ID + name agreement)

**Files:**
- Modify: `internal/scan/match.go`
- Test: `internal/scan/match_test.go`

**Interfaces:**
- Consumes: `matchOCRID(ocrID string, roster []RosterEntry)`, `matchOCRName(ocrName string, roster []RosterEntry)` — both existing, both fail-safe (exact normalized ID; unique normalized name, duplicates fail). Their old return type `(Proposal, bool)` was deleted in Task 1 — restore them returning `(int64, bool)` (the roster DB id) as part of this task.
- Produces:

```go
// StudentMatch is the page-level student resolution (spec §6, D64).
type StudentMatch struct {
	AgreedID       int64  // both rungs independently resolve here; 0 = no auto-assign
	ProposedID     int64  // partial signal for orphan pre-fill; 0 = none
	ProposalSource string // "ocr_agree" | "ocr_id" | "ocr_name" | "ocr_disagree" | ""
}

func MatchStudent(ocrID, ocrName string, roster []RosterEntry) StudentMatch
```

Callers pass `""` for a field whose legibility flag is false — an illegible read participates in no rung.

- [ ] **Step 1: Write the failing test**

Append to `internal/scan/match_test.go` (roster entries are synthetic — never real students):

```go
func TestMatchStudent(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902001", Name: "王小明"},
		{ID: 2, ExternalID: "B11902002", Name: "李大華"},
		{ID: 3, ExternalID: "B11902003", Name: "陳同名"},
		{ID: 4, ExternalID: "B11902004", Name: "陳同名"}, // duplicate name: name rung must fail
	}
	cases := []struct {
		name string
		id   string
		nm   string
		want StudentMatch
	}{
		{"agree", "B11902001", "王小明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"agree with noise", " b11902001 ", "王 小 明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"id only (name illegible)", "B11902002", "", StudentMatch{ProposedID: 2, ProposalSource: "ocr_id"}},
		{"id only (name unknown)", "B11902002", "無此人", StudentMatch{ProposedID: 2, ProposalSource: "ocr_id"}},
		{"name only", "", "李大華", StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}},
		{"disagreement", "B11902001", "李大華", StudentMatch{ProposalSource: "ocr_disagree"}},
		{"duplicate name never matches", "B11902003", "陳同名", StudentMatch{ProposedID: 3, ProposalSource: "ocr_id"}},
		{"one digit off never matches", "B11902009", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_name"}},
		{"nothing", "", "", StudentMatch{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchStudent(tc.id, tc.nm, roster)
			if got != tc.want {
				t.Fatalf("MatchStudent(%q, %q) = %+v, want %+v", tc.id, tc.nm, got, tc.want)
			}
		})
	}
}
```

Note the two load-bearing cases: `duplicate name never matches` proves a clean ID with an ambiguous name is an orphan-with-prefill, never an auto-assign; `one digit off never matches` proves there is no fuzzy ID rung.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan -run TestMatchStudent -v`
Expected: FAIL — `undefined: StudentMatch` / `undefined: MatchStudent`

- [ ] **Step 3: Implement**

In `internal/scan/match.go`, make `matchOCRID`/`matchOCRName` return `(int64, bool)` (roster DB id + hit) and add:

```go
// MatchStudent resolves one page's student from the two independent OCR reads
// (spec §6, D64): auto-assign eligibility (AgreedID) requires the ID rung and the
// name rung to independently resolve to the SAME live student. The ID rung is
// exact-only — one OCR digit error is exactly how a page lands on the wrong real
// student, so there is deliberately no fuzzy ID matching. Anything less than
// agreement yields at most a pre-fill proposal for the orphan queue.
func MatchStudent(ocrID, ocrName string, roster []RosterEntry) StudentMatch {
	idHit, idOK := matchOCRID(ocrID, roster)
	nameHit, nameOK := matchOCRName(ocrName, roster)
	switch {
	case idOK && nameOK && idHit == nameHit:
		return StudentMatch{AgreedID: idHit, ProposedID: idHit, ProposalSource: "ocr_agree"}
	case idOK && nameOK:
		// ID says one student, name says another — possibly a student who wrote
		// someone else's ID. Flag distinctly, pre-fill nothing (spec §6).
		return StudentMatch{ProposalSource: "ocr_disagree"}
	case idOK:
		return StudentMatch{ProposedID: idHit, ProposalSource: "ocr_id"}
	case nameOK:
		return StudentMatch{ProposedID: nameHit, ProposalSource: "ocr_name"}
	default:
		return StudentMatch{}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/scan -v`
Expected: PASS — including the pre-existing `matchOCRID`/`matchOCRName`/normalization tables (update their assertions to the `(int64, bool)` returns).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/match.go internal/scan/match_test.go
git commit -m "feat(scan): strict ID+name agreement matcher for page assignment"
```

---

### Task 4: Batch creation + zip expansion onto scan_sources

**Files:**
- Create: `internal/scan/batch.go`
- Test: `internal/scan/batch_test.go` (new integration-style test file; uses `storetest.Fresh`, skips without `ADAMARKER_TEST_DATABASE_URL`)

**Interfaces:**
- Consumes: slim `Service` from Task 1 (`storeEntryBlob`, `openZip`, `acceptZipEntry`, `readZipEntry`, `acceptedExt`, `baseName`, `MaxSourceBytes`, `MaxZipBytes`, `MaxEntryBytes`); queries `CreateScanBatch`, `CreateScanSource`, `SetBatchSourceRef`, `ListIDRegions`, `GetScanBatch`, `ListScanSourcesForBatch`.
- Produces:

```go
type NewBatch struct {
	OCREnabled  bool
	OCRProvider string
	OCRModel    string
}

type SourceUpload struct {
	Filename string
	R        io.Reader // streamed to the blob store; never fully buffered by the caller
}

type BatchView struct {
	Batch   db.ScanBatch
	Created int        // sources created
	Skipped []SkipInfo // reasons: unknown_extension | empty | too_large | duplicate
}

// CreateBatch validates the three ID regions exist, creates the batch, stores
// each source (streamed), and enqueues scan.split per source (loose files) or
// one scan.expand (zip). Exactly one of sources/zip must be non-empty.
func (s *Service) CreateBatch(ctx context.Context, assessmentID int64, nb NewBatch, sources []SourceUpload, zip io.Reader, actor int64) (BatchView, error)

// Expand is the scan.expand worker body: unzip the batch's stored archive into
// scan_sources rows (images and PDFs), then enqueue scan.split per source.
func (s *Service) Expand(ctx context.Context, batchID int64) error

// ErrRegionsIncomplete is returned by CreateBatch when the assessment does not
// have all three id-region kinds drawn.
var ErrRegionsIncomplete = errors.New("scan: draw the three ID regions (student ID, name, problem) before uploading scans")
```

Blob keys: sources are NOT content-addressed in the key (the sha isn't known until the stream is consumed) — `fmt.Sprintf("assessments/%d/scans/%d/src/%d.%s", assessmentID, batchID, ordinal, ext)`; `Put` returns the sha used for the `UNIQUE(batch_id, source_sha256)` dedupe; a duplicate row (`pgx.ErrNoRows` from `CreateScanSource`) deletes the just-stored blob and records skip `"duplicate"`. Size cap: wrap in `io.LimitReader(r, MaxSourceBytes+1)` for PDFs (`MaxEntryBytes+1` for loose images), check `Put`'s returned size, delete + skip `"too_large"` when over.

- [ ] **Step 1: Write the failing tests**

Create `internal/scan/batch_test.go` with the fixture pattern from the deleted `scan_test.go` (this fixture is reused by Tasks 5–8, so write it once here):

```go
package scan

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

type fx struct {
	svc *Service
	st  *store.Store
	aid int64
	ctx context.Context
	// recorded enqueues
	splits    *[]int64
	renders   *[]renderChunk
	identifies *[]int64
	promotes  *[]PromotePage
}

type renderChunk struct {
	SourceID int64
	PageIDs  []int64
}

func setup(t *testing.T) fx {
	t.Helper()
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ing := &ingest.Service{Store: st, Blobs: blobs, Renderer: render.NewFake(1)}
	svc := &Service{Store: st, Blobs: blobs, Renderer: render.NewFake(3), Ingest: ing}
	f := fx{svc: svc, st: st, ctx: context.Background(),
		splits: &[]int64{}, renders: &[]renderChunk{}, identifies: &[]int64{}, promotes: &[]PromotePage{}}
	svc.EnqueueSplit = func(_ context.Context, _ pgx.Tx, ids []int64) error {
		*f.splits = append(*f.splits, ids...)
		return nil
	}
	svc.EnqueueRenderPages = func(_ context.Context, _ pgx.Tx, sourceID int64, pageIDs []int64) error {
		*f.renders = append(*f.renders, renderChunk{SourceID: sourceID, PageIDs: pageIDs})
		return nil
	}
	svc.EnqueueIdentifyPages = func(_ context.Context, _ pgx.Tx, ids []int64) error {
		*f.identifies = append(*f.identifies, ids...)
		return nil
	}
	svc.EnqueuePromotePages = func(_ context.Context, _ pgx.Tx, items []PromotePage) error {
		*f.promotes = append(*f.promotes, items...)
		return nil
	}

	// assessment + 3 problems + roster (synthetic 9-char NTU-format ids so the
	// local-OCR PickID length-5 floor accepts them; names are synthetic).
	a, err := st.Q.CreateAssessment(f.ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Scan Exam"})
	if err != nil {
		t.Fatal(err)
	}
	f.aid = a.ID
	for n := 1; n <= 3; n++ {
		if _, err := st.Q.CreateProblem(f.ctx, db.CreateProblemParams{
			AssessmentID: f.aid, Number: int32(n), Title: "Q", MaxPoints: "10",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct{ ext, name string }{
		{"B11902001", "王小明"}, {"B11902002", "李大華"},
	} {
		if _, err := st.Q.UpsertStudent(f.ctx, db.UpsertStudentParams{
			StudentID: s.ext, Name: s.name, Email: s.ext + "@x.edu",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// NOTE: verify CreateAssessment/CreateProblem/UpsertStudent param structs
// against internal/store/db before running — copy exact field names from the
// generated code (they exist; the old scan_test.go used the same fixtures).

func addRegions(f fx, t *testing.T) {
	t.Helper()
	for _, k := range []string{"student_id", "name", "problem_id"} {
		if _, err := f.st.Q.CreateIDRegion(f.ctx, db.CreateIDRegionParams{
			AssessmentID: f.aid, Kind: k,
			X: 0.05, Y: 0.02, W: 0.25, H: 0.06, Color: "#4a4a4a", Padding: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCreateBatch_RequiresAllThreeRegions(t *testing.T) {
	f := setup(t)
	// only two kinds drawn
	for _, k := range []string{"student_id", "name"} {
		if _, err := f.st.Q.CreateIDRegion(f.ctx, db.CreateIDRegionParams{
			AssessmentID: f.aid, Kind: k, X: 0.05, Y: 0.02, W: 0.25, H: 0.06,
			Color: "#4a4a4a", Padding: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "run1.pdf", R: strings.NewReader("%PDF-1")},
	}, nil, 1)
	if err != ErrRegionsIncomplete {
		t.Fatalf("want ErrRegionsIncomplete, got %v", err)
	}
}

func TestCreateBatch_LoosePDFs_CreatesSourcesAndEnqueuesSplit(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"},
		[]SourceUpload{
			{Filename: "run1.pdf", R: strings.NewReader("%PDF-1 run one")},
			{Filename: "run2.pdf", R: strings.NewReader("%PDF-1 run two")},
			{Filename: "notes.txt", R: strings.NewReader("nope")},
		}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 2 {
		t.Fatalf("created = %d, want 2", view.Created)
	}
	if len(view.Skipped) != 1 || view.Skipped[0].Reason != "unknown_extension" {
		t.Fatalf("skipped = %+v", view.Skipped)
	}
	if len(*f.splits) != 2 {
		t.Fatalf("split enqueues = %d, want 2", len(*f.splits))
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("sources = %d (%v), want 2", len(srcs), err)
	}
	if srcs[0].SourceKind != "pdf" {
		t.Fatalf("kind = %s", srcs[0].SourceKind)
	}
}

func TestCreateBatch_DuplicateSourceSkipped(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	same := "%PDF-1 identical bytes"
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "a.pdf", R: strings.NewReader(same)},
		{Filename: "b.pdf", R: strings.NewReader(same)},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 1 || len(view.Skipped) != 1 || view.Skipped[0].Reason != "duplicate" {
		t.Fatalf("view = %+v", view)
	}
}

func TestCreateBatch_SourceOverCap(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	old := MaxSourceBytes
	MaxSourceBytes = 16
	t.Cleanup(func() { MaxSourceBytes = old })
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "huge.pdf", R: strings.NewReader("%PDF-1 way past the sixteen byte cap")},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 0 || len(view.Skipped) != 1 || view.Skipped[0].Reason != "too_large" {
		t.Fatalf("view = %+v", view)
	}
}

func TestExpand_ZipIntoSources(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	z := zipOf(t, map[string][]byte{
		"page-001.png":       pngBytes(t, 40, 60),
		"run.pdf":            []byte("%PDF-1 zipped run"),
		"__MACOSX/junk.png":  {1, 2, 3},
		"cover.txt":          []byte("skip me"),
	})
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, nil, bytes.NewReader(z), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Expand(f.ctx, view.Batch.ID); err != nil {
		t.Fatal(err)
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2 (png + pdf; macosx + txt skipped)", len(srcs))
	}
	if len(*f.splits) != 2 {
		t.Fatalf("split enqueues = %d, want 2", len(*f.splits))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make db-test-up && ADAMARKER_TEST_DATABASE_URL="postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable" go test ./internal/scan -run 'TestCreateBatch|TestExpand' -v`
Expected: FAIL — `undefined: NewBatch` / `undefined: SourceUpload` etc. (If the fixture's `CreateAssessment`/`CreateProblem`/`UpsertStudent` param fields don't match the generated code, fix the fixture from `internal/store/db` — those queries predate this plan.)

- [ ] **Step 3: Implement `internal/scan/batch.go`**

```go
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// NewBatch are the operator-chosen OCR options for one uploaded scanner run.
type NewBatch struct {
	OCREnabled  bool
	OCRProvider string
	OCRModel    string
}

// SourceUpload is one uploaded source file, streamed — CreateBatch never buffers
// a whole scanner PDF in memory.
type SourceUpload struct {
	Filename string
	R        io.Reader
}

// BatchView reports one CreateBatch call's outcome.
type BatchView struct {
	Batch   db.ScanBatch
	Created int
	Skipped []SkipInfo
}

// ErrRegionsIncomplete: identification is impossible without all three region
// kinds, so the batch is rejected at the door instead of half-running (spec §5).
var ErrRegionsIncomplete = errors.New("scan: draw the three ID regions (student ID, name, problem) before uploading scans")

const requiredRegionKinds = 3

func (s *Service) regionsComplete(ctx context.Context, assessmentID int64) error {
	regions, err := s.Store.Q.ListIDRegions(ctx, assessmentID)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, r := range regions {
		kinds[r.Kind] = true
	}
	if len(kinds) != requiredRegionKinds {
		return ErrRegionsIncomplete
	}
	return nil
}

// CreateBatch creates the batch row plus its sources. Loose sources enqueue one
// scan.split each; a zip stores the archive and enqueues one scan.expand.
func (s *Service) CreateBatch(ctx context.Context, assessmentID int64, nb NewBatch, sources []SourceUpload, zip io.Reader, actor int64) (BatchView, error) {
	if (len(sources) == 0) == (zip == nil) {
		return BatchView{}, errors.New("scan: provide loose sources or a zip, not both or neither")
	}
	if err := s.regionsComplete(ctx, assessmentID); err != nil {
		return BatchView{}, err
	}
	batch, err := s.Store.Q.CreateScanBatch(ctx, db.CreateScanBatchParams{
		AssessmentID: assessmentID,
		OcrEnabled:   nb.OCREnabled,
		OcrProvider:  textOrNull(nb.OCRProvider),
		OcrModel:     textOrNull(nb.OCRModel),
		SourceRef:    textOrNull(""),
		CreatedBy:    int8OrNull(actor),
	})
	if err != nil {
		return BatchView{}, fmt.Errorf("scan: create batch: %w", err)
	}
	if zip != nil {
		return s.createZipBatch(ctx, batch, zip)
	}
	return s.createLooseBatch(ctx, batch, sources)
}

func (s *Service) createLooseBatch(ctx context.Context, batch db.ScanBatch, sources []SourceUpload) (BatchView, error) {
	view := BatchView{Batch: batch}
	var sourceIDs []int64
	for n, up := range sources {
		ext, ok := acceptedExt(up.Filename)
		if !ok {
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "unknown_extension"})
			continue
		}
		cap := MaxSourceBytes // scanner PDFs
		kind := "pdf"
		if ext != "pdf" {
			cap = int64(MaxEntryBytes) // loose page images
			kind = "image"
		}
		key := fmt.Sprintf("assessments/%d/scans/%d/src/%d.%s", batch.AssessmentID, batch.ID, n, ext)
		sha, size, err := s.Blobs.Put(ctx, key, io.LimitReader(up.R, cap+1))
		if err != nil {
			return view, fmt.Errorf("scan: store source: %w", err)
		}
		if size == 0 {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "empty"})
			continue
		}
		if size > cap {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "too_large"})
			continue
		}
		src, err := s.Store.Q.CreateScanSource(ctx, db.CreateScanSourceParams{
			BatchID: batch.ID, OriginalFilename: baseName(up.Filename),
			SourceRef: key, SourceSha256: sha, SourceKind: kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = s.Blobs.Delete(ctx, key)
			view.Skipped = append(view.Skipped, SkipInfo{Filename: up.Filename, Reason: "duplicate"})
			continue
		}
		if err != nil {
			return view, fmt.Errorf("scan: record source: %w", err)
		}
		view.Created++
		sourceIDs = append(sourceIDs, src.ID)
	}
	if len(sourceIDs) > 0 && s.EnqueueSplit != nil {
		if err := s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueSplit(ctx, tx, sourceIDs)
		}); err != nil {
			return view, fmt.Errorf("scan: enqueue split: %w", err)
		}
	}
	return view, nil
}

func (s *Service) createZipBatch(ctx context.Context, batch db.ScanBatch, zip io.Reader) (BatchView, error) {
	view := BatchView{Batch: batch}
	key := fmt.Sprintf("assessments/%d/scans/%d/upload.zip", batch.AssessmentID, batch.ID)
	_, size, err := s.Blobs.Put(ctx, key, io.LimitReader(zip, MaxZipBytes+1))
	if err != nil {
		return view, fmt.Errorf("scan: store zip: %w", err)
	}
	if size > MaxZipBytes {
		_ = s.Blobs.Delete(ctx, key)
		return view, errors.New("scan: zip exceeds the archive size cap")
	}
	if err := s.Store.Q.SetBatchSourceRef(ctx, db.SetBatchSourceRefParams{ID: batch.ID, SourceRef: textOrNull(key)}); err != nil {
		return view, fmt.Errorf("scan: record zip ref: %w", err)
	}
	if s.EnqueueExpand != nil {
		if err := s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueExpand(ctx, tx, batch.ID)
		}); err != nil {
			return view, fmt.Errorf("scan: enqueue expand: %w", err)
		}
	}
	return view, nil
}

// Expand is the scan.expand worker body. Idempotent: re-delivery re-reads the
// zip; existing sources dedupe on (batch_id, source_sha256).
func (s *Service) Expand(ctx context.Context, batchID int64) error {
	batch, err := s.Store.Q.GetScanBatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("scan: expand: load batch: %w", err)
	}
	if !batch.SourceRef.Valid || batch.SourceRef.String == "" {
		return nil // loose batch; nothing to expand
	}
	ra, size, closeFn, err := s.openZip(ctx, batch.SourceRef.String)
	if err != nil {
		return fmt.Errorf("scan: expand: open zip: %w", err)
	}
	defer closeFn()
	zr, err := zipReader(ra, size)
	if err != nil {
		return fmt.Errorf("scan: expand: read zip: %w", err)
	}
	var sourceIDs []int64
	n := 0
	for _, entry := range zr.File {
		if !acceptZipEntry(entry) {
			continue
		}
		ext, ok := acceptedExt(entry.Name)
		if !ok {
			continue
		}
		data, err := readZipEntry(entry)
		if err != nil || len(data) == 0 || len(data) > MaxEntryBytes {
			continue
		}
		key, sha, kind, err := s.storeEntryBlob(ctx, batch.AssessmentID, batch.ID, data, ext)
		if err != nil {
			return fmt.Errorf("scan: expand: store entry: %w", err)
		}
		src, err := s.Store.Q.CreateScanSource(ctx, db.CreateScanSourceParams{
			BatchID: batch.ID, OriginalFilename: baseName(entry.Name),
			SourceRef: key, SourceSha256: sha, SourceKind: kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // duplicate under redelivery
		}
		if err != nil {
			return fmt.Errorf("scan: expand: record entry: %w", err)
		}
		sourceIDs = append(sourceIDs, src.ID)
		n++
	}
	if len(sourceIDs) > 0 && s.EnqueueSplit != nil {
		return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueSplit(ctx, tx, sourceIDs)
		})
	}
	return nil
}
```

Adapt the kept helpers where signatures drifted: the old `openZip` returned `(io.ReaderAt, int64, func(), error)` and the caller built `zip.NewReader` inline — add a two-line `func zipReader(ra io.ReaderAt, size int64) (*zip.Reader, error) { return zip.NewReader(ra, size) }` or inline it; `readZipEntry`/`storeEntryBlob` keep their old bodies (`storeEntryBlob` returns content-addressed entry keys `assessments/%d/scans/%d/%s.%s` — fine for zip entries whose bytes are already in memory).

- [ ] **Step 4: Run tests to verify they pass**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestCreateBatch|TestExpand' -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/scan/batch.go internal/scan/batch_test.go
git commit -m "feat(scan): batch creation with streamed sources and zip expansion"
```

---

### Task 5: Split sources into pages + chunked render with three crops

**Files:**
- Create: `internal/scan/pages.go`
- Test: `internal/scan/pages_test.go`

**Interfaces:**
- Consumes: fixture from Task 4 (`setup`, `addRegions`, `pngBytes`); `render.Renderer.Open` → `Document.PageCount()/RenderPageImage(ctx, idx, s.Opts)`; `ingest.NormalizeImageRaster(data, s.Opts)`; `imaging.CropImage(raster, []imaging.Region{...}, cropQuality)`; queries `GetScanSource`, `CreateScanPage`, `ListScanPagesForSource`, `SetScanPageRendered`, `SetScanSourcePageCount`, `SetScanSourceError`, `ListIDRegions`.
- Produces:

```go
// SplitSource is the scan.split worker body: count pages, insert one scan_pages
// row per page, fan out scan.render_pages in chunks of renderChunkSize.
func (s *Service) SplitSource(ctx context.Context, sourceID int64) error

// RenderPages is the scan.render_pages worker body: one Renderer.Open per chunk
// (F3), render each page image + the three region crops, enqueue identify.
func (s *Service) RenderPages(ctx context.Context, sourceID int64, pageIDs []int64) error

// regionByKind maps the assessment's typed id_regions to imaging.Regions.
func regionByKind(regions []db.IDRegion) map[string]imaging.Region
```

Blob keys: page image `assessments/%d/scans/%d/pages/%d-%s.jpg` (assessmentID, batchID, pageID, sha8); crops `assessments/%d/scans/%d/idcrop/%d-%s-%s.jpg` (assessmentID, batchID, pageID, kind, sha8) — the `/idcrop/` segment is load-bearing (LoadIDCrop gate). Identify is enqueued only when all three crops rendered AND (batch.OcrEnabled OR s.Local != nil) — D19: no crop, no identify, ever. Deterministic decode failures stamp the error column and return nil (no retry); infra errors return the error (River retries).

- [ ] **Step 1: Write the failing tests**

Create `internal/scan/pages_test.go`:

```go
package scan

import (
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// looseSource creates a batch with one loose source and returns the source row.
func looseSource(f fx, t *testing.T, filename, content string, nb NewBatch) db.ScanSource {
	t.Helper()
	view, err := f.svc.CreateBatch(f.ctx, f.aid, nb, []SourceUpload{
		{Filename: filename, R: strings.NewReader(content)},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources: %d, %v", len(srcs), err)
	}
	return srcs[0]
}

func TestSplitSource_PDFCreatesPageRowsAndChunks(t *testing.T) {
	f := setup(t) // fake renderer: 3 pages
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1 three pages", NewBatch{})

	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages = %d (%v), want 3", len(pages), err)
	}
	if len(*f.renders) != 1 || len((*f.renders)[0].PageIDs) != 3 {
		t.Fatalf("render chunks = %+v, want one chunk of 3", *f.renders)
	}
	// idempotent under redelivery
	*f.renders = nil
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, _ = f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if len(pages) != 3 {
		t.Fatalf("pages after redelivery = %d, want 3", len(pages))
	}
}

func TestSplitSource_ImageIsOnePage(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "page.png", string(pngBytes(t, 40, 60)), NewBatch{})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	pages, _ := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
}

func TestRenderPages_StoresImageAndThreeCrops(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, _ := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	for _, p := range pages {
		if !p.ImageRef.Valid || !p.StudentIDCropRef.Valid || !p.NameCropRef.Valid || !p.ProblemCropRef.Valid {
			t.Fatalf("page %d missing render outputs: %+v", p.ID, p)
		}
		if !strings.Contains(p.StudentIDCropRef.String, "/idcrop/") {
			t.Fatalf("crop key must contain /idcrop/: %s", p.StudentIDCropRef.String)
		}
		if !p.ImageSha256.Valid || p.ImageSha256.String == "" {
			t.Fatalf("page %d missing image sha", p.ID)
		}
	}
	if len(*f.identifies) != len(pages) {
		t.Fatalf("identify enqueues = %d, want %d", len(*f.identifies), len(pages))
	}
}

func TestRenderPages_OCRDisabledNoLocal_NoIdentify(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	src := looseSource(f, t, "run.pdf", "%PDF-1", NewBatch{OCREnabled: false})
	_ = f.svc.SplitSource(f.ctx, src.ID)
	chunk := (*f.renders)[0]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	if len(*f.identifies) != 0 {
		t.Fatalf("identify enqueues = %d, want 0", len(*f.identifies))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestSplitSource|TestRenderPages' -v`
Expected: FAIL — `undefined: (*Service).SplitSource`

- [ ] **Step 3: Implement `internal/scan/pages.go`**

```go
package scan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// SplitSource counts the source's pages and materializes one scan_pages row per
// page. Idempotent: CreateScanPage no-ops on (source_id, page_index) conflicts,
// and rows are re-listed before the render fan-out so redelivery re-enqueues
// every page (render itself is idempotent).
func (s *Service) SplitSource(ctx context.Context, sourceID int64) error {
	src, err := s.Store.Q.GetScanSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: split: load source: %w", err)
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, src.BatchID)
	if err != nil {
		return fmt.Errorf("scan: split: load batch: %w", err)
	}

	pageCount := 1
	if src.SourceKind == "pdf" {
		data, err := s.readAllSource(ctx, src.SourceRef)
		if err != nil {
			return fmt.Errorf("scan: split: read source: %w", err)
		}
		doc, err := s.Renderer.Open(ctx, data)
		if err != nil {
			// Deterministic: a corrupt PDF never gets better on retry.
			_ = s.Store.Q.SetScanSourceError(ctx, db.SetScanSourceErrorParams{
				ID: sourceID, Error: textOrNull("source is not a readable PDF"),
			})
			return nil
		}
		pageCount = doc.PageCount()
		_ = doc.Close()
		if pageCount == 0 {
			_ = s.Store.Q.SetScanSourceError(ctx, db.SetScanSourceErrorParams{
				ID: sourceID, Error: textOrNull("source PDF has no pages"),
			})
			return nil
		}
	}

	for i := 0; i < pageCount; i++ {
		_, err := s.Store.Q.CreateScanPage(ctx, db.CreateScanPageParams{
			SourceID: sourceID, BatchID: src.BatchID,
			AssessmentID: batch.AssessmentID, PageIndex: int32(i),
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("scan: split: create page: %w", err)
		}
	}
	if err := s.Store.Q.SetScanSourcePageCount(ctx, db.SetScanSourcePageCountParams{
		ID: sourceID, PageCount: int4Of(pageCount),
	}); err != nil {
		return fmt.Errorf("scan: split: record page count: %w", err)
	}

	pages, err := s.Store.Q.ListScanPagesForSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: split: list pages: %w", err)
	}
	if s.EnqueueRenderPages == nil {
		return nil
	}
	return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		for start := 0; start < len(pages); start += renderChunkSize {
			end := min(start+renderChunkSize, len(pages))
			ids := make([]int64, 0, end-start)
			for _, p := range pages[start:end] {
				if p.ImageRef.Valid { // already rendered on a prior delivery
					continue
				}
				ids = append(ids, p.ID)
			}
			if len(ids) == 0 {
				continue
			}
			if err := s.EnqueueRenderPages(ctx, tx, sourceID, ids); err != nil {
				return err
			}
		}
		return nil
	})
}

// readAllSource reads a source blob bounded by MaxSourceBytes (PDF sources may
// far exceed the per-entry cap readAll enforces).
func (s *Service) readAllSource(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxSourceBytes+1))
}

// RenderPages renders one chunk of a source's pages: page JPG + the three region
// crops per page, then enqueues identify for the chunk.
func (s *Service) RenderPages(ctx context.Context, sourceID int64, pageIDs []int64) error {
	src, err := s.Store.Q.GetScanSource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: render: load source: %w", err)
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, src.BatchID)
	if err != nil {
		return fmt.Errorf("scan: render: load batch: %w", err)
	}
	regions, err := s.Store.Q.ListIDRegions(ctx, batch.AssessmentID)
	if err != nil {
		return fmt.Errorf("scan: render: load regions: %w", err)
	}
	byKind := regionByKind(regions)
	if len(byKind) != requiredRegionKinds {
		// Regions were deleted between upload and render; deterministic.
		for _, id := range pageIDs {
			s.setPageError(ctx, id, "id regions incomplete; redraw and retry")
		}
		return nil
	}

	data, err := s.readAllSource(ctx, src.SourceRef)
	if err != nil {
		return fmt.Errorf("scan: render: read source: %w", err)
	}

	var doc render.Document
	if src.SourceKind == "pdf" {
		doc, err = s.Renderer.Open(ctx, data)
		if err != nil {
			for _, id := range pageIDs {
				s.setPageError(ctx, id, "source is not a readable PDF")
			}
			return nil
		}
		defer doc.Close()
	}

	var identify []int64
	for _, pageID := range pageIDs {
		page, err := s.Store.Q.GetScanPage(ctx, pageID)
		if err != nil {
			return fmt.Errorf("scan: render: load page: %w", err)
		}
		if page.ImageRef.Valid { // idempotent redelivery
			continue
		}

		var raster image.Image
		var pg render.Page
		if src.SourceKind == "pdf" {
			raster, pg, err = doc.RenderPageImage(ctx, int(page.PageIndex), s.Opts)
		} else {
			raster, pg, err = ingest.NormalizeImageRaster(data, s.Opts)
		}
		if err != nil {
			if isInterruption(ctx, err) {
				return err
			}
			s.setPageError(ctx, pageID, "page render failed")
			continue
		}

		pageKey := fmt.Sprintf("assessments/%d/scans/%d/pages/%d-%s.jpg",
			batch.AssessmentID, batch.ID, pageID, pg.SHA256[:8])
		if _, _, err := s.Blobs.Put(ctx, pageKey, bytes.NewReader(pg.JPEG)); err != nil {
			return fmt.Errorf("scan: render: store page image: %w", err)
		}

		cropRefs := map[string]string{}
		cropFailed := false
		for _, kind := range []string{"student_id", "name", "problem_id"} {
			crop, err := imaging.CropImage(raster, []imaging.Region{byKind[kind]}, cropQuality)
			if err != nil {
				s.setPageError(ctx, pageID, "region crop failed; check the drawn regions")
				cropFailed = true
				break
			}
			key := fmt.Sprintf("assessments/%d/scans/%d/idcrop/%d-%s-%s.jpg",
				batch.AssessmentID, batch.ID, pageID, kind, crop.SHA256()[:8])
			if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(crop.JPEG())); err != nil {
				return fmt.Errorf("scan: render: store crop: %w", err)
			}
			cropRefs[kind] = key
		}
		if cropFailed {
			continue
		}

		if err := s.Store.Q.SetScanPageRendered(ctx, db.SetScanPageRenderedParams{
			ID: pageID, ImageRef: textOrNull(pageKey), ImageSha256: textOrNull(pg.SHA256),
			ImageWidth: int4Of(pg.Width), ImageHeight: int4Of(pg.Height),
			StudentIDCropRef: textOrNull(cropRefs["student_id"]),
			NameCropRef:      textOrNull(cropRefs["name"]),
			ProblemCropRef:   textOrNull(cropRefs["problem_id"]),
		}); err != nil {
			return fmt.Errorf("scan: render: record: %w", err)
		}
		identify = append(identify, pageID)
	}

	if len(identify) > 0 && (batch.OcrEnabled || s.Local != nil) && s.EnqueueIdentifyPages != nil {
		return s.Store.WithTxPgx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return s.EnqueueIdentifyPages(ctx, tx, identify)
		})
	}
	return nil
}

// regionByKind converts the typed id_regions rows to imaging.Regions keyed by kind.
func regionByKind(regions []db.IDRegion) map[string]imaging.Region {
	m := make(map[string]imaging.Region, len(regions))
	for _, r := range regions {
		m[r.Kind] = imaging.Region{
			X: float64(r.X), Y: float64(r.Y), W: float64(r.W), H: float64(r.H),
			Color: r.Color, Padding: float64(r.Padding),
		}
	}
	return m
}
```

(Complete the imports: `bytes`, `errors`, `image`, `io`, `github.com/HaoWen46/adagrade/internal/render`. `min` is a Go 1.21 builtin.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestSplitSource|TestRenderPages' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/scan/pages.go internal/scan/pages_test.go
git commit -m "feat(scan): split sources into page rows; chunked render with three region crops"
```

---

### Task 6: Page identification — OCR, matching, auto-assign / orphan / park

The heart of the feature. One vision call per page carrying all three crops; strict resolution; never overwrite an occupied cell.

**Files:**
- Create: `internal/scan/identify.go`
- Test: `internal/scan/identify_test.go`

**Interfaces:**
- Consumes: `MatchStudent` (Task 3), `ParseProblemRef` (Task 2), `PickID`/`PickName` (localocr.go), `imaging.LoadIDCrop`, `llm.Request{ToolName: "submit_identity", …}` via `s.Providers.Provider(ctx, name)`, `ocr.Reader` (s.Local), queries `GetScanPage`, `SetScanPageIdentified`, `AssignScanPage`, `ParkScanPage`, `LivePageForCell`, `GetActiveSubmissionForProblem`, `ListActiveStudents`, `ListProblems`.
- Produces:

```go
// IdentifyPage is the scan.identify_page worker body.
func (s *Service) IdentifyPage(ctx context.Context, pageID int64, finalAttempt bool) error

// pageIdentityOut is the strict OCR schema (DisallowUnknownFields on parse).
type pageIdentityOut struct {
	StudentID        string `json:"student_id"`
	Name             string `json:"name"`
	Problem          string `json:"problem"`
	StudentIDLegible bool   `json:"student_id_legible"`
	NameLegible      bool   `json:"name_legible"`
	ProblemLegible   bool   `json:"problem_legible"`
}

// cellIncumbent reports what already occupies a (student, problem) cell:
// a live assigned page (possibly promoted), or a live submission with no page.
func (s *Service) cellIncumbent(ctx context.Context, assessmentID, studentID, problemID, excludePageID int64) (page *db.ScanPage, sub *db.Submission, err error)

// placeAuto applies one identified page's resolution in a single transaction:
// empty cell -> auto-assign; occupied -> park duplicate/conflict; no resolution
// -> orphan with proposal. Exposed for tests as a method used by IdentifyPage.
func (s *Service) placeAuto(ctx context.Context, page db.ScanPage, out pageIdentityOut, engine string) error
```

Cloud prompt/schema (constants in identify.go):

```go
var pageIdentitySchema = []byte(`{"type":"object","additionalProperties":false,"properties":{"student_id":{"type":"string"},"name":{"type":"string"},"problem":{"type":"string"},"student_id_legible":{"type":"boolean"},"name_legible":{"type":"boolean"},"problem_legible":{"type":"boolean"}},"required":["student_id","name","problem","student_id_legible","name_legible","problem_legible"]}`)

const (
	pageIDSystemPrompt = "You read three tightly-cropped header boxes from one exam page, in order: (1) student ID box, (2) name box (often Chinese), (3) problem number box (like Q1 / P3 / 問2). Transcribe exactly what is written in each. Do not guess, translate, or infer. For any box that is blank or unreadable, set its legible flag to false and return an empty string."
	pageIDUserPrompt   = "Return the student ID, name, and problem reference exactly as written in these three boxes, in order."
	pageIDMaxTokens    = 256
)
```

Error taxonomy mirrors the old IdentifyFile exactly: `isInterruption` → return err (never terminal, F17); `*llm.ProviderUnavailableError` → terminal `"OCR provider unavailable"`; `retryableError` → return err unless finalAttempt then terminal `"OCR failed after retries"`; malformed-after-re-ask → terminal `"OCR produced no usable identity"`. Local rung first (D24): `ReadLines` per crop, `PickID`/`PickName`/best-line→`ParseProblemRef`; local succeeds only if it yields a full auto-assignable resolution (agreement + valid problem), else fall through to cloud when `batch.OcrEnabled`.

- [ ] **Step 1: Write the failing tests**

Create `internal/scan/identify_test.go`. Reuse the Task 4 fixture; add the scripted-provider helper (pattern from the deleted scan_test.go — `fake.ScriptedProvider` + `llm.StaticSource`):

```go
package scan

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// identityJSON builds the provider's scripted response.
func identityJSON(id, name, problem string, idLeg, nameLeg, probLeg bool) string {
	return fmt.Sprintf(`{"student_id":%q,"name":%q,"problem":%q,"student_id_legible":%v,"name_legible":%v,"problem_legible":%v}`,
		id, name, problem, idLeg, nameLeg, probLeg)
}

// renderedPage drives upload->split->render for one single-page PDF source and
// returns the page row, with the scripted provider wired as "p".
func renderedPage(f fx, t *testing.T, prov llm.Provider) db.ScanPage {
	t.Helper()
	f.svc.Providers = llm.StaticSource{"p": prov}
	f.svc.Renderer = render.NewFake(1)
	src := looseSource(f, t, "run.pdf", "%PDF-1 "+t.Name(), NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[len(*f.renders)-1]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages: %d, %v", len(pages), err)
	}
	return pages[0]
}

func student(f fx, t *testing.T, ext string) db.Student {
	t.Helper()
	st, err := f.st.Q.GetStudentByExternalID(f.ctx, ext)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func problemByNumber(f fx, t *testing.T, n int32) db.Problem {
	t.Helper()
	probs, err := f.st.Q.ListProblems(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		if p.Number == n {
			return p
		}
	}
	t.Fatalf("no problem %d", n)
	return db.Problem{}
}

func TestIdentifyPage_AgreementAutoAssigns(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q2", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("not auto-assigned: %+v", got)
	}
	if got.AssignedProblemID.Int64 != problemByNumber(f, t, 2).ID {
		t.Fatalf("wrong problem: %+v", got)
	}
	if got.AssignedBy.Valid {
		t.Fatal("auto-assign must leave assigned_by NULL")
	}
}

func TestIdentifyPage_CleanIDIllegibleName_Orphans(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "", "Q1", true, false, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("clean ID with illegible name must NOT auto-assign (user rule)")
	}
	if got.ProposalSource.String != "ocr_id" || got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("want ocr_id pre-fill, got %+v", got)
	}
	if !got.ProposedProblemID.Valid {
		t.Fatal("valid problem read should still be pre-filled")
	}
	if !got.IdentifiedAt.Valid {
		t.Fatal("identified_at must be stamped (orphan, not processing)")
	}
}

func TestIdentifyPage_DisagreementOrphansNoPrefill(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "李大華", "Q1", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid || got.ProposedStudentID.Valid {
		t.Fatalf("disagreement must not assign or pre-fill a student: %+v", got)
	}
	if got.ProposalSource.String != "ocr_disagree" {
		t.Fatalf("want ocr_disagree flag, got %q", got.ProposalSource.String)
	}
}

func TestIdentifyPage_InvalidProblemOrphans(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q9", true, true, true)}, // only 3 problems exist
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("out-of-range problem must not auto-assign")
	}
	if got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatal("student agreement should still pre-fill")
	}
}

func TestIdentifyPage_OccupiedCell_IdenticalParksDuplicate(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: script}
	first := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	// Re-upload: same source content in a NEW batch -> same rendered image sha.
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	// force identical image sha (fake renderer derives pixels from page index,
	// so two page-0 renders are byte-identical already; assert to be safe)
	p1, _ := f.st.Q.GetScanPage(f.ctx, first.ID)
	p2, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if p1.ImageSha256.String != p2.ImageSha256.String {
		t.Skip("fake renderer no longer deterministic; adjust fixture")
	}
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("occupied cell must never be overwritten")
	}
	if got.ParkedReason.String != "duplicate" || got.ParkedAgainst.Int64 != first.ID {
		t.Fatalf("want duplicate park against first page, got %+v", got)
	}
	// incumbent untouched
	keep, _ := f.st.Q.GetScanPage(f.ctx, first.ID)
	if !keep.AssignedStudentID.Valid {
		t.Fatal("incumbent lost its assignment")
	}
}

func TestIdentifyPage_OccupiedCell_DifferentParksConflict(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov1 := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)},
	}}
	first := renderedPage(f, t, prov1)
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	// different pixels: use the 2-page fake so page index differs? Simpler:
	// overwrite the second page's image_sha256 directly to simulate different
	// content OCRing to the same cell.
	prov2 := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)},
	}}
	second := renderedPage(f, t, prov2)
	mustExecPage(f, t, second.ID, "UPDATE scan_pages SET image_sha256 = 'different' WHERE id = $1")
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if got.ParkedReason.String != "conflict" {
		t.Fatalf("want conflict park, got %+v", got)
	}
}

func mustExecPage(f fx, t *testing.T, id int64, sql string) {
	t.Helper()
	if _, err := f.st.Pool.Exec(f.ctx, sql, id); err != nil {
		t.Fatal(err)
	}
}

func TestIdentifyPage_ProviderUnavailable_Terminal(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	// point the batch at an unknown provider name
	f.svc.Providers = llm.StaticSource{}
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal("provider-unavailable is terminal, not retryable")
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.Error.String != "OCR provider unavailable" {
		t.Fatalf("error = %q", got.Error.String)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run TestIdentifyPage -v`
Expected: FAIL — `undefined: (*Service).IdentifyPage`

- [ ] **Step 3: Implement `internal/scan/identify.go`**

Structure (verbatim skeleton — flesh out with the exact taxonomy noted in Interfaces):

```go
// IdentifyPage OCRs one page's three crops and applies the resolution. Skips
// silently when the page has no crops, is already assigned/parked/discarded/
// promoted, or the batch has OCR disabled and no local reader exists.
func (s *Service) IdentifyPage(ctx context.Context, pageID int64, finalAttempt bool) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil { return fmt.Errorf("scan: identify: load page: %w", err) }
	if page.AssignedStudentID.Valid || page.DiscardedAt.Valid ||
		page.SubmissionID.Valid || page.ParkedReason.Valid ||
		!page.StudentIDCropRef.Valid || !page.NameCropRef.Valid || !page.ProblemCropRef.Valid {
		return nil
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, page.BatchID)
	if err != nil { return fmt.Errorf("scan: identify: load batch: %w", err) }

	crops, err := s.loadCrops(ctx, page) // [3]imaging.IDCrop via LoadIDCrop; a gate failure is a bug -> terminal
	if err != nil { s.setPageError(ctx, pageID, "crop unreadable"); return nil }

	if s.Local != nil {
		if done := s.identifyLocal(ctx, page, crops); done {
			return nil
		}
	}
	if !batch.OcrEnabled {
		// local rung failed / absent and cloud is off: orphan with empty reads.
		return s.placeAuto(ctx, page, pageIdentityOut{}, engineLocal)
	}
	out, engine, err := s.identifyCloud(ctx, batch, crops, finalAttempt)
	if err != nil { /* taxonomy: interruption -> err; unavailable -> terminal nil;
	                   retryable -> err unless finalAttempt (then terminal nil);
	                   malformed -> terminal nil */ }
	return s.placeAuto(ctx, page, out, engine)
}
```

`identifyLocal`: `ReadLines` each crop, filter `Confidence >= localOCRMinConfidence`; `id := PickID(idLines)`, `name := PickName(nameLines)`, `problem :=` highest-confidence line text of the problem crop. Build `pageIdentityOut{StudentID: id, Name: name, Problem: problem, StudentIDLegible: id != "", NameLegible: name != "", ProblemLegible: problem != ""}`; resolve with `MatchStudent` + `ParseProblemRef`; return done=true (and `placeAuto(... engineLocal)`) ONLY when the resolution is a full auto-assign; any failure or partial → return false, fall to cloud (log Warn with page_id only — no OCR text).

`identifyCloud`: mirrors the old cloud rung verbatim (limiter wait, `provider.Grade(ctx, batch.OcrModel.String, llm.Request{System: pageIDSystemPrompt, Prompt: prompt, Images: []imaging.ProviderImage{crops[0], crops[1], crops[2]}, Schema: pageIdentitySchema, Temperature: 0, MaxTokens: pageIDMaxTokens, ToolName: "submit_identity", ReasoningLevel: "off"})`, strict unmarshal with `DisallowUnknownFields`, one re-ask on parse failure).

`placeAuto` (single `WithTx`):

```go
func (s *Service) placeAuto(ctx context.Context, page db.ScanPage, out pageIdentityOut, engine string) error {
	roster, err := s.roster(ctx) // ListActiveStudents -> []RosterEntry
	if err != nil { return retryableError{err} }
	problems, err := s.Store.Q.ListProblems(ctx, page.AssessmentID)
	if err != nil { return retryableError{err} }

	ocrID, ocrName, ocrProblem := "", "", ""
	if out.StudentIDLegible { ocrID = out.StudentID }
	if out.NameLegible { ocrName = out.Name }
	if out.ProblemLegible { ocrProblem = out.Problem }

	m := MatchStudent(ocrID, ocrName, roster)
	var problemID int64
	if n, ok := ParseProblemRef(ocrProblem); ok {
		for _, p := range problems {
			if int(p.Number) == n { problemID = p.ID; break }
		}
	}

	return s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := q.SetScanPageIdentified(ctx, db.SetScanPageIdentifiedParams{
			ID: page.ID,
			OcrStudentID: textOrNull(out.StudentID), OcrName: textOrNull(out.Name),
			OcrProblem: textOrNull(out.Problem),
			OcrStudentIDLegible: boolOf(out.StudentIDLegible),
			OcrNameLegible: boolOf(out.NameLegible), OcrProblemLegible: boolOf(out.ProblemLegible),
			OcrEngine: textOrNull(engine),
			ProposedStudentID: int8OrNull(m.ProposedID),
			ProposedProblemID: int8OrNull(problemID),
			ProposalSource: textOrNull(m.ProposalSource),
		}); err != nil {
			return err
		}
		if m.AgreedID == 0 || problemID == 0 {
			return nil // orphan
		}
		incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, m.AgreedID, problemID, page.ID)
		if err != nil { return err }
		switch {
		case incPage != nil:
			reason := "conflict"
			if incPage.ImageSha256.Valid && page.ImageSha256.Valid &&
				incPage.ImageSha256.String == page.ImageSha256.String {
				reason = "duplicate"
			}
			return q.ParkScanPage(ctx, db.ParkScanPageParams{
				ID: page.ID, ParkedReason: textOrNull(reason), ParkedAgainst: int8OrNull(incPage.ID),
			})
		case incSub != nil:
			// A submission with no page (Submissions-tab upload): content can't
			// be compared cheaply -> conflict, incumbent referenced by nothing.
			return q.ParkScanPage(ctx, db.ParkScanPageParams{
				ID: page.ID, ParkedReason: textOrNull("conflict"), ParkedAgainst: int8OrNull(0),
			})
		default:
			return q.AssignScanPage(ctx, db.AssignScanPageParams{
				ID: page.ID, AssignedStudentID: int8OrNull(m.AgreedID),
				AssignedProblemID: int8OrNull(problemID), AssignedBy: int8OrNull(0), // NULL = auto
			})
		}
	})
}
```

`cellIncumbentQ(ctx, q, …)` runs `LivePageForCell` (`pgx.ErrNoRows` → nil) then `GetActiveSubmissionForProblem` with `ProblemID: pgtype.Int8{Int64: problemID, Valid: true}` and `GetActiveSubmission` (whole-assessment) — either live submission occupies the cell. If `AssignScanPage` loses a race to the partial unique index (`*pgconn.PgError` code `23505`), re-run the incumbent lookup and park instead — wrap that retry inside `placeAuto` after the tx returns the violation.

- [ ] **Step 4: Run tests to verify they pass**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run TestIdentifyPage -v`
Expected: PASS (7 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/scan/identify.go internal/scan/identify_test.go
git commit -m "feat(scan): page identification — 3-crop OCR, strict matching, park-not-overwrite"
```

---

### Task 7: Manual page mutations

**Files:**
- Create: `internal/scan/mutations.go`
- Test: `internal/scan/mutations_test.go`

**Interfaces:**
- Consumes: `cellIncumbentQ` (Task 6), `ingest.RetractSubmission`, queries `AssignScanPage`, `UnassignScanPage`, `DiscardScanPage`, `UndiscardScanPage`, `ClearScanPageError`, `SetScanPageForcePromote`, `GetScanPage`, `GetStudent`.
- Produces:

```go
// ErrCellOccupied is the manual-assign 409: the target cell already holds a live
// page or submission. IncumbentPageID==0 means the incumbent is a submission.
type ErrCellOccupied struct {
	IncumbentPageID       int64
	IncumbentSubmissionID int64
	Duplicate             bool // content-identical to the incumbent page
}
func (e *ErrCellOccupied) Error() string

// ErrPagePromoted blocks mutating a page whose submission already exists.
type ErrPagePromoted struct{ PageID int64 }
func (e *ErrPagePromoted) Error() string

// AssignPage manually assigns a page to a (student, problem) cell.
// studentID/problemID are DB ids. Occupied cells return *ErrCellOccupied —
// the UI resolves via ResolveConflict; manual assignment NEVER overwrites.
func (s *Service) AssignPage(ctx context.Context, pageID, studentID, problemID, actor int64) error

func (s *Service) UnassignPage(ctx context.Context, pageID, actor int64) error
func (s *Service) DiscardPage(ctx context.Context, pageID int64, reason string, actor int64) error
func (s *Service) UndiscardPage(ctx context.Context, pageID, actor int64) error

// RetryPage clears the error and re-enqueues the right stage: identify when
// crops exist, else a render chunk for just this page.
func (s *Service) RetryPage(ctx context.Context, pageID, actor int64) error

// ResolveConflict resolves a parked page. action "keep" discards the parked
// page; action "replace" takes the cell: an unpromoted incumbent page is
// unassigned (back to orphan), a promoted one has its submission retracted
// (force applies ingest's graded guard) before the parked page is assigned;
// force also marks the page force_promote so the next finalize can re-ingest
// over grading records.
func (s *Service) ResolveConflict(ctx context.Context, pageID int64, action string, force bool, actor int64) error
```

Validations in AssignPage: page not promoted (`ErrPagePromoted`), student active + not withdrawn, problem belongs to the assessment. All mutations audit with counts/ids only (`s.Store.InsertAudit(ctx, actor, "scan.page.assign", "scan_page", itoa(pageID), map[string]any{"student_id": studentID, "problem_id": problemID})` — internal ids, no names).

- [ ] **Step 1: Write the failing tests**

`internal/scan/mutations_test.go` — table of behaviors (reuse fixture + `renderedPage` from identify_test):

```go
func TestAssignPage_EmptyCellAssigns(t *testing.T)        // manual assign sets assigned_by = actor
func TestAssignPage_OccupiedCellErrCellOccupied(t *testing.T) // assign 2nd page to same cell -> *ErrCellOccupied with IncumbentPageID + Duplicate flag
func TestAssignPage_PromotedPageBlocked(t *testing.T)     // stamp submission_id -> *ErrPagePromoted
func TestAssignPage_WithdrawnStudentRejected(t *testing.T)
func TestUnassignPage_ClearsCellAndForcePromote(t *testing.T)
func TestDiscardUndiscard_RoundTrip(t *testing.T)
func TestResolveConflict_KeepDiscardsParked(t *testing.T)
func TestResolveConflict_ReplaceUnpromotedIncumbent(t *testing.T) // incumbent -> orphan (assigned cleared), parked page -> assigned to cell
func TestResolveConflict_ReplacePromotedIncumbent_RetractsAndAssigns(t *testing.T) // incumbent promoted via fake ingest; after replace: submission retracted, parked page assigned, force_promote set when force
```

Write each test against the produced signatures above; drive cell setup with `IdentifyPage` (agreement script) or direct `AssignPage` calls. For the promoted-incumbent case, promote via Task 8's PromotePage if already merged, else stamp `submission_id` through a real `ingest.Ingest` call with `Kind: "image"` on the incumbent's page image and `SetScanPageSubmission` (three lines; keeps the retract path honest).

- [ ] **Step 2: Run tests to verify they fail**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestAssignPage|TestUnassignPage|TestDiscardUndiscard|TestResolveConflict' -v`
Expected: FAIL — undefined methods

- [ ] **Step 3: Implement `internal/scan/mutations.go`**

AssignPage core:

```go
func (s *Service) AssignPage(ctx context.Context, pageID, studentID, problemID, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil { return fmt.Errorf("scan: assign: load page: %w", err) }
	if page.SubmissionID.Valid { return &ErrPagePromoted{PageID: pageID} }
	student, err := s.Store.Q.GetStudent(ctx, studentID)
	if err != nil { return errors.New("scan: no such student") }
	if student.WithdrawnAt.Valid { return errors.New("scan: student is withdrawn") }
	problem, err := s.Store.Q.GetProblem(ctx, problemID)
	if err != nil || problem.AssessmentID != page.AssessmentID {
		return errors.New("scan: problem does not belong to this assessment")
	}
	return s.Store.WithTx(ctx, func(q *db.Queries) error {
		incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, studentID, problemID, pageID)
		if err != nil { return err }
		if incPage != nil {
			return &ErrCellOccupied{
				IncumbentPageID: incPage.ID,
				Duplicate: incPage.ImageSha256.Valid && page.ImageSha256.Valid &&
					incPage.ImageSha256.String == page.ImageSha256.String,
			}
		}
		if incSub != nil {
			return &ErrCellOccupied{IncumbentSubmissionID: incSub.ID}
		}
		if err := q.AssignScanPage(ctx, db.AssignScanPageParams{
			ID: pageID, AssignedStudentID: int8OrNull(studentID),
			AssignedProblemID: int8OrNull(problemID), AssignedBy: int8OrNull(actor),
		}); err != nil {
			return err
		}
		return nil
	})
}
```

ResolveConflict replace path (promoted incumbent):

```go
	if incPage.SubmissionID.Valid {
		if err := s.Ingest.RetractSubmission(ctx, incPage.SubmissionID.Int64, actor, force); err != nil &&
			!errors.Is(err, ingest.ErrAlreadyRetracted) {
			return err
		}
	}
	// tx: unassign incumbent (clears its cell + submission link stays for audit? no —
	// UnassignScanPage clears assignment; SetScanPageSubmission(0) is NOT called: the
	// retracted submission id remains on the incumbent as history), assign parked page,
	// stamp force_promote when force.
```

Inside one `WithTx`: `q.UnassignScanPage(incumbent)`, then `q.AssignScanPage(parked page, cell, actor)`, then if `force` `q.SetScanPageForcePromote(pageID, true)`. NOTE — the incumbent keeps `submission_id` pointing at a retracted submission; the derived state must therefore check *retracted* promotion: add to the derived-state rule (Task 10) that `submission_id` only means "promoted" while the linked submission is live; simpler and chosen here: clear the link with `q.SetScanPageSubmission(ctx, db.SetScanPageSubmissionParams{ID: incPage.ID, SubmissionID: pgtype.Int8{}})` so the incumbent returns to orphan cleanly.

- [ ] **Step 4: Run tests to verify they pass**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestAssignPage|TestUnassignPage|TestDiscardUndiscard|TestResolveConflict' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/scan/mutations.go internal/scan/mutations_test.go
git commit -m "feat(scan): manual page mutations with occupied-cell protection"
```

---

### Task 8: Assessment-wide finalize + per-page promotion + mask seeding

**Files:**
- Create: `internal/scan/finalize.go`
- Test: `internal/scan/finalize_test.go`

**Interfaces:**
- Consumes: `ingest.Ingest(ctx, assessmentID, IngestInput{Filename, Data, Kind: "image", TargetProblemID}, uploadedBy, force) FileResult`; queries `CountMissingCells`, `ListMissingCells`, `ListAssignedUnpromotedPages`, `ClearPromotionErrorsForAssessment`, `SetScanPageSubmission`, `SetScanPagePromotionError`, `ListIDRegions`, `ListMaskRegions`, `CreateMaskRegion`.
- Produces:

```go
type FinalizeReport struct {
	Enqueued        int  `json:"enqueued"`
	AlreadyPromoted int  `json:"already_promoted"`
	MissingCells    int  `json:"missing_cells"`
}

type ErrMissingUnacknowledged struct{ Count int }
func (e *ErrMissingUnacknowledged) Error() string

// Finalize is assessment-wide and incremental: gate on missing cells (unless
// acked), seed identity mask regions, enqueue one scan.promote_page per
// assigned-unpromoted page. Safe to re-run; only new pages promote.
func (s *Service) Finalize(ctx context.Context, assessmentID int64, ackMissing bool, actor int64) (FinalizeReport, error)

// PromotePage is the scan.promote_page worker body: page image -> per-problem
// image submission through the ingest seam (supersede chain, graded/published
// guards unchanged).
func (s *Service) PromotePage(ctx context.Context, pageID int64, force bool, actor int64, finalAttempt bool) error

// seedMaskRegions copies the student_id and name id_regions into mask_regions
// with page_scope 'all' (idempotent by exact rect equality) — every page now
// carries identity at known coordinates, so masking hides it everywhere (D66).
// The problem_id region is NOT seeded (the grader may use it).
func (s *Service) seedMaskRegions(ctx context.Context, assessmentID int64) error
```

Promotion detail: `Filename: student.StudentID + ".jpg"` (page images are always JPEG), `Data:` the page-image blob via `readAll`, `Kind: "image"`, `TargetProblemID: page.AssignedProblemID.Int64`, `force: force || page.ForcePromote`. Result taxonomy mirrors the old PromoteFile: `Status == "ingested"` → `SetScanPageSubmission`; `"rejected"`/`"quarantined"` → `setPromotionError(ctx, pageID, "promotion rejected: "+res.Reason)` and return nil (business outcome, not a retry); interruption → return err; other errors → err unless finalAttempt (then terminal via setPromotionError).

- [ ] **Step 1: Write the failing tests**

`internal/scan/finalize_test.go`:

```go
func TestFinalize_MissingCellsGate(t *testing.T)
// 2 students x 3 problems, nothing assigned -> Finalize(ack=false) returns
// *ErrMissingUnacknowledged{Count: 6}; Finalize(ack=true) succeeds with
// MissingCells: 6, Enqueued: 0.

func TestFinalize_EnqueuesAssignedUnpromoted(t *testing.T)
// auto-assign one page (identify script), Finalize(ack=true): Enqueued == 1;
// drive PromotePage on the recorded item; then re-run Finalize: Enqueued == 0,
// AlreadyPromoted == 1 (incremental).

func TestPromotePage_CreatesPerProblemImageSubmission(t *testing.T)
// after PromotePage: page.submission_id set; the submission row has
// source_kind='image', problem_id = the assigned problem; answer_pages row
// exists for (student, problem) answer.

func TestPromotePage_WholeAssessmentSubmissionCoexists(t *testing.T)
// student already has a live whole-assessment submission (via ingest.IngestFile
// with the fake renderer) -> promoting a page for problem 2 supersedes it per
// ingest's whole-scope supersede rules; assert ingest result is "ingested".
// (This documents ingest's existing behavior: per-problem ingest supersedes at
// problem scope only; the whole-assessment submission REMAINS live. Assert what
// ingest actually does — read the guards in internal/ingest/ingest.go:263-288 —
// and pin it here so the matrix's "covered" semantics stay truthful.)

func TestFinalize_SeedsMaskRegions(t *testing.T)
// after Finalize: mask_regions contains the student_id and name rects with
// page_scope='all'; problem rect NOT copied; second Finalize does not duplicate.

func TestPromotePage_ForcePromoteFlagPassesForce(t *testing.T)
// grade the incumbent (insert a grading record via the fixture pattern from the
// old scan_test.go), set force_promote on the page, PromotePage with force=false
// -> ingest still replaces because page.ForcePromote carries the force.
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -run 'TestFinalize|TestPromotePage' -v`
Expected: FAIL — undefined methods

- [ ] **Step 3: Implement `internal/scan/finalize.go`**

```go
func (s *Service) Finalize(ctx context.Context, assessmentID int64, ackMissing bool, actor int64) (FinalizeReport, error) {
	missing, err := s.Store.Q.CountMissingCells(ctx, assessmentID)
	if err != nil {
		return FinalizeReport{}, fmt.Errorf("scan: finalize: count missing: %w", err)
	}
	if missing > 0 && !ackMissing {
		return FinalizeReport{}, &ErrMissingUnacknowledged{Count: int(missing)}
	}
	if err := s.seedMaskRegions(ctx, assessmentID); err != nil {
		return FinalizeReport{}, fmt.Errorf("scan: finalize: seed masks: %w", err)
	}
	report := FinalizeReport{MissingCells: int(missing)}
	err = s.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.ClearPromotionErrorsForAssessment(ctx, assessmentID); err != nil {
			return err
		}
		pages, err := q.ListAssignedUnpromotedPages(ctx, assessmentID)
		if err != nil {
			return err
		}
		items := make([]PromotePage, 0, len(pages))
		for _, p := range pages {
			items = append(items, PromotePage{PageID: p.ID, Force: p.ForcePromote, Actor: actor})
		}
		report.Enqueued = len(items)
		if len(items) > 0 && s.EnqueuePromotePages != nil {
			return s.EnqueuePromotePages(ctx, tx, items)
		}
		return nil
	})
	if err != nil {
		return FinalizeReport{}, err
	}
	// AlreadyPromoted = live assigned pages minus the ones just enqueued.
	assigned, err := s.Store.Q.ListLiveAssignedPagesForAssessment(ctx, assessmentID)
	if err == nil {
		for _, p := range assigned {
			if p.SubmissionID.Valid {
				report.AlreadyPromoted++
			}
		}
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.finalize", "assessment", itoa(assessmentID),
		map[string]any{"enqueued": report.Enqueued, "missing_cells": report.MissingCells})
	return report, nil
}
```

`seedMaskRegions`: `ListIDRegions` → for kinds `student_id`/`name`, `ListMaskRegions(assessmentID)` and skip when an existing region has `PageScope == "all"` and identical `X, Y, W, H` (float32 equality is exact — both sides come from the same stored row); else `CreateMaskRegion(db.CreateMaskRegionParams{AssessmentID, PageScope: "all", X: r.X, Y: r.Y, W: r.W, H: r.H, Color: r.Color, Padding: r.Padding})`.

`PromotePage`:

```go
func (s *Service) PromotePage(ctx context.Context, pageID int64, force bool, actor int64, finalAttempt bool) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil { return fmt.Errorf("scan: promote: load page: %w", err) }
	if page.SubmissionID.Valid || !page.AssignedStudentID.Valid || page.DiscardedAt.Valid {
		return nil // idempotent / no longer eligible
	}
	student, err := s.Store.Q.GetStudent(ctx, page.AssignedStudentID.Int64)
	if err != nil { return fmt.Errorf("scan: promote: load student: %w", err) }
	img, err := s.readAll(ctx, page.ImageRef.String)
	if err != nil {
		if isInterruption(ctx, err) { return err }
		if finalAttempt { s.setPromotionError(ctx, pageID, "promotion rejected: page image unreadable"); return nil }
		return err
	}
	res := s.Ingest.Ingest(ctx, page.AssessmentID, ingest.IngestInput{
		Filename: student.StudentID + ".jpg", Data: img,
		Kind: "image", TargetProblemID: page.AssignedProblemID.Int64,
	}, actor, force || page.ForcePromote)
	switch res.Status {
	case "ingested":
		return s.Store.Q.SetScanPageSubmission(ctx, db.SetScanPageSubmissionParams{
			ID: pageID, SubmissionID: int8OrNull(res.SubmissionID),
		})
	default: // rejected | quarantined: business outcome, never retried
		s.setPromotionError(ctx, pageID, "promotion rejected: "+res.Reason)
		return nil
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/scan -v`
Expected: PASS — the whole package.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/finalize.go internal/scan/finalize_test.go
git commit -m "feat(scan): assessment-wide incremental finalize, per-page promotion, mask seeding"
```

---

### Task 9: River jobs and wiring

**Files:**
- Modify: `internal/queue/river.go`
- Test: `internal/queue/river_test.go`

**Interfaces:**
- Consumes: service methods `Expand` (existing wire), `SplitSource`, `RenderPages`, `IdentifyPage`, `PromotePage`; seams `EnqueueSplit`, `EnqueueRenderPages`, `EnqueueIdentifyPages`, `EnqueuePromotePages` (Task 1 struct).
- Produces (kind strings are NEW — old in-flight jobs with the retired shapes can't mis-decode):

```go
// ScanSplitArgs splits one uploaded source into scan_pages rows (D14: IDs only).
type ScanSplitArgs struct {
	SourceID int64 `json:"source_id"`
}
func (ScanSplitArgs) Kind() string { return "scan.split" }
func (ScanSplitArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanSplitMaxAttempts}
}

// ScanRenderPagesArgs renders one chunk of a source's pages (one PDFium
// document open per chunk).
type ScanRenderPagesArgs struct {
	SourceID int64   `json:"source_id"`
	PageIDs  []int64 `json:"page_ids"`
}
func (ScanRenderPagesArgs) Kind() string { return "scan.render_pages" }
func (ScanRenderPagesArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanRenderMaxAttempts}
}

// ScanIdentifyPageArgs OCRs one page's three crops (rides the llm queue —
// shares provider rate limiting).
type ScanIdentifyPageArgs struct {
	PageID int64 `json:"page_id"`
}
func (ScanIdentifyPageArgs) Kind() string { return "scan.identify_page" }
func (ScanIdentifyPageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: llmQueue, MaxAttempts: scanIdentifyMaxAttempts}
}

// ScanPromotePageArgs promotes one assigned page at finalize.
type ScanPromotePageArgs struct {
	PageID int64 `json:"page_id"`
	Force  bool  `json:"force"`
	Actor  int64 `json:"actor"`
}
func (ScanPromotePageArgs) Kind() string { return "scan.promote_page" }
func (ScanPromotePageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanPromoteMaxAttempts}
}
```

Constants restored to the grouped block: `scanSplitMaxAttempts = 3`, `scanRenderMaxAttempts = 3`, `scanIdentifyMaxAttempts = 3`, `scanPromoteMaxAttempts = 3`.

- [ ] **Step 1: Write the failing tests**

Extend `internal/queue/river_test.go`'s `TestArgsKindsAndInsertOpts` table with the four new kinds (`checkInsertOpts` asserts queue + max attempts), and the closure-wiring test to assert `EnqueueSplit`, `EnqueueRenderPages`, `EnqueueIdentifyPages`, `EnqueuePromotePages` are non-nil after `queue.New` with `Deps{Scans: &scan.Service{}}`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue -run 'TestArgsKinds|TestNew_Scans' -v`
Expected: FAIL — undefined arg types / nil closures

- [ ] **Step 3: Implement**

Workers follow the house pattern exactly (struct + `WorkerDefaults` + `client *Client` + `scans *scan.Service`, body wrapped in `snoozeOnShutdown`, `final := job.Attempt >= <const>` where the service takes it):

```go
type splitWorker struct {
	river.WorkerDefaults[ScanSplitArgs]
	client *Client
	scans  *scan.Service
}
func (w *splitWorker) Work(ctx context.Context, job *river.Job[ScanSplitArgs]) error {
	return w.client.snoozeOnShutdown(w.scans.SplitSource(ctx, job.Args.SourceID))
}
func (w *splitWorker) Timeout(*river.Job[ScanSplitArgs]) time.Duration { return 5 * time.Minute }

type renderPagesWorker struct {
	river.WorkerDefaults[ScanRenderPagesArgs]
	client *Client
	scans  *scan.Service
}
func (w *renderPagesWorker) Work(ctx context.Context, job *river.Job[ScanRenderPagesArgs]) error {
	return w.client.snoozeOnShutdown(w.scans.RenderPages(ctx, job.Args.SourceID, job.Args.PageIDs))
}
func (w *renderPagesWorker) Timeout(*river.Job[ScanRenderPagesArgs]) time.Duration { return 10 * time.Minute }

type identifyPageWorker struct {
	river.WorkerDefaults[ScanIdentifyPageArgs]
	client *Client
	scans  *scan.Service
}
func (w *identifyPageWorker) Work(ctx context.Context, job *river.Job[ScanIdentifyPageArgs]) error {
	final := job.Attempt >= scanIdentifyMaxAttempts
	return w.client.snoozeOnShutdown(w.scans.IdentifyPage(ctx, job.Args.PageID, final))
}
func (w *identifyPageWorker) Timeout(*river.Job[ScanIdentifyPageArgs]) time.Duration { return 5 * time.Minute }

type promotePageWorker struct {
	river.WorkerDefaults[ScanPromotePageArgs]
	client *Client
	scans  *scan.Service
}
func (w *promotePageWorker) Work(ctx context.Context, job *river.Job[ScanPromotePageArgs]) error {
	final := job.Attempt >= scanPromoteMaxAttempts
	return w.client.snoozeOnShutdown(w.scans.PromotePage(ctx, job.Args.PageID, job.Args.Force, job.Args.Actor, final))
}
func (w *promotePageWorker) Timeout(*river.Job[ScanPromotePageArgs]) time.Duration { return 5 * time.Minute }
```

Enqueue helpers mirror `enqueueScanRenderTx`'s `InsertManyParams` loop + empty-guard + `InsertManyFastTx`: `enqueueScanSplitTx(ctx, tx, sourceIDs []int64)`, `enqueueScanRenderPagesTx(ctx, tx, sourceID int64, pageIDs []int64)` (single `InsertTx` — one chunk per call), `enqueueScanIdentifyPagesTx(ctx, tx, pageIDs []int64)`, `enqueueScanPromotePagesTx(ctx, tx, items []scan.PromotePage)`. Register the four workers inside the existing `if deps.Scans != nil` block and wire the four seams next to `EnqueueExpand`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/queue -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/queue/river.go internal/queue/river_test.go
git commit -m "feat(queue): page-level scan jobs (split, render_pages, identify_page, promote_page)"
```

---

### Task 10: HTTP API for pages, matrix, and finalize

**Files:**
- Modify: `internal/httpapi/scans.go` (grows back from id-regions-only to the full page surface)
- Modify: `internal/httpapi/api.go` (route block)
- Test: `internal/httpapi/scans_test.go` (recreated)

**Interfaces:**
- Consumes: everything Tasks 4–8 produced; house helpers `pathID`, `apiError`, `writeJSON`, `decodeJSON`, `currentUser`, `extendBodyDeadline`, `uploadBodyDeadline`, `s.streamBlob`, `s.audit`, `int8Of`.
- Produces routes (replaces the Task 1 stub block in api.go):

```go
	// Scan intake (page-level, design spec 2026-07-04).
	api.HandleFunc("POST /api/assessments/{id}/scan-batches", s.handleCreateScanBatch)
	api.HandleFunc("GET /api/assessments/{id}/scan-batches", s.handleListScanBatches)
	api.HandleFunc("GET /api/assessments/{id}/scan-pages", s.handleListScanPages)
	api.HandleFunc("GET /api/assessments/{id}/scan-matrix", s.handleScanMatrix)
	api.HandleFunc("POST /api/assessments/{id}/scan-finalize", s.handleScanFinalize)
	api.HandleFunc("GET /api/assessments/{id}/scan-missing", s.handleScanMissing)
	api.HandleFunc("POST /api/scan-pages/{id}/assign", s.handleAssignScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/unassign", s.handleUnassignScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/discard", s.handleDiscardScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/undiscard", s.handleUndiscardScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/retry", s.handleRetryScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/resolve-conflict", s.handleResolveScanPageConflict)
	api.HandleFunc("GET /api/scan-pages/{id}/image", s.handleScanPageImage)
	api.HandleFunc("GET /api/scan-pages/{id}/crop", s.handleScanPageCrop)
	api.HandleFunc("GET /api/assessments/{id}/id-regions", s.handleGetIDRegions)
	api.HandleFunc("PUT /api/assessments/{id}/id-regions", s.handlePutIDRegions)
```

JSON shapes:

```go
// deriveScanPageState precedence (D2):
// error > discarded > promoted > parked > assigned > orphan > processing
// (orphan = identified_at set, nothing assigned; processing = not yet identified)
func deriveScanPageState(r db.ScanPageRowsRow) string

type scanPageJSON struct {
	ID                 int64  `json:"id"`
	BatchID            int64  `json:"batch_id"`
	PageIndex          int32  `json:"page_index"`
	State              string `json:"state"` // processing|orphan|assigned|parked|promoted|discarded|error
	Error              string `json:"error,omitempty"`
	OCRStudentID       string `json:"ocr_student_id,omitempty"`
	OCRName            string `json:"ocr_name,omitempty"`
	OCRProblem         string `json:"ocr_problem,omitempty"`
	OCREngine          string `json:"ocr_engine,omitempty"`
	ProposalSource     string `json:"proposal_source,omitempty"`
	ProposedExternalID string `json:"proposed_student_id,omitempty"`
	ProposedName       string `json:"proposed_name,omitempty"`
	ProposedProblemID  int64  `json:"proposed_problem_id,omitempty"`
	AssignedExternalID string `json:"assigned_student_id,omitempty"`
	AssignedName       string `json:"assigned_name,omitempty"`
	AssignedProblemID  int64  `json:"assigned_problem_id,omitempty"`
	AssignedByUser     bool   `json:"assigned_by_user"` // false = auto-assigned
	ParkedReason       string `json:"parked_reason,omitempty"`
	ParkedAgainst      int64  `json:"parked_against,omitempty"`
	DiscardReason      string `json:"discard_reason,omitempty"`
	HasImage           bool   `json:"has_image"`
}

type scanBatchJSON struct {
	ID           int64  `json:"id"`
	AssessmentID int64  `json:"assessment_id"`
	OCREnabled   bool   `json:"ocr_enabled"`
	OCRProvider  string `json:"ocr_provider,omitempty"`
	OCRModel     string `json:"ocr_model,omitempty"`
}

type scanBatchListRowJSON struct {
	scanBatchJSON
	TotalPages      int `json:"total_pages"`
	ProcessingPages int `json:"processing_pages"`
	OrphanPages     int `json:"orphan_pages"`
	AssignedPages   int `json:"assigned_pages"` // assigned + promoted
	ParkedPages     int `json:"parked_pages"`
	DiscardedPages  int `json:"discarded_pages"`
	ErroredPages    int `json:"errored_pages"`
}

type matrixCellJSON struct {
	ProblemID int64  `json:"problem_id"`
	State     string `json:"state"` // empty|auto|manual|promoted|submitted
	PageID    int64  `json:"page_id,omitempty"`
}
type matrixRowJSON struct {
	StudentID string           `json:"student_id"` // external id
	Name      string           `json:"name"`
	Cells     []matrixCellJSON `json:"cells"`
}
type matrixJSON struct {
	Problems []int64          `json:"problems"` // column order (problem ids by number)
	Rows     []matrixRowJSON  `json:"rows"`
}
```

Handler behaviors:
- `handleCreateScanBatch`: multipart; `extendBodyDeadline(w, uploadBodyDeadline)`, `r.ParseMultipartForm(64 << 20)` (threshold only — big parts spill to disk temp files; each part is then STREAMED `fh.Open()` → `scan.SourceUpload{R: f}` without ReadAll). Fields `ocr_enabled` (`!= "0"`), `ocr_provider`, `ocr_model`; file fields `files` (many) xor `zip` (one) — same 400s as the old handler. `scan.ErrRegionsIncomplete` → 409 `{"error": …, "regions_incomplete": true}`. Audit `"scan.batch.create"`. 200 `{"batch": scanBatchJSON, "created": N, "skipped": []}`.
- `handleListScanBatches`: `ListScanBatches` + one `ScanBatchPageProgress(ctx, batchIDs)` (no N+1, no PII columns). 200 `{"batches": [...]}`.
- `handleListScanPages`: `ScanPageRows(ctx, aid)` → map to `scanPageJSON` with `deriveScanPageState`; optional `?state=` filter applied server-side. 200 `{"pages": [...]}`.
- `handleScanMatrix`: compose from `ListActiveStudents` + `ListProblems` + `ListLiveAssignedPagesForAssessment` + `ListLiveSubmissionsForAssessment`. Cell state: page with submission → `promoted`; page assigned_by NULL → `auto`, else `manual`; no page but live submission covering (per-problem match or whole-assessment row) → `submitted`; else `empty`. 200 `matrixJSON`.
- `handleScanFinalize`: body `{"ack_missing": bool}`; `*scan.ErrMissingUnacknowledged` → 409 `{"error":…, "missing_cells": N}`; success 202 with the `FinalizeReport`.
- `handleScanMissing`: `ListMissingCells` → 200 `{"missing": [{"student_id":…, "name":…, "problem_number":…}]}` (PII to staff session only — mirrors the old missing list).
- `handleAssignScanPage`: body `{"student_id": "<external>", "problem_id": <int64>}`; resolve external via `GetStudentByExternalID` (404 `"no such student"`); `*scan.ErrCellOccupied` → 409 `{"error":…, "incumbent_page_id":…, "incumbent_submission_id":…, "duplicate": bool}`; `*scan.ErrPagePromoted` → 409 `{"error":…, "promoted": true}`. 200 `{"assigned": true}`.
- `handleResolveScanPageConflict`: body `{"action": "keep"|"replace", "force": bool}`; 400 on unknown action. 200 `{"resolved": true}`.
- `handleUnassign/Discard/Undiscard/Retry`: mirror the old per-file handlers (body `{"reason": string}` for discard; 204 on success for unassign/undiscard/retry, 200 `{"discarded": true}` for discard).
- `handleScanPageImage`: `GetScanPage` → 404s → `s.streamBlob(w, r, page.ImageRef.String, "image/jpeg")`.
- `handleScanPageCrop`: `?kind=student_id|name|problem_id` (400 otherwise) → stream the matching crop ref.

- [ ] **Step 1: Write the failing integration tests**

Recreate `internal/httpapi/scans_test.go` on the harness (`harnessEnv`, `scanSetup` — reuse the old `scanSetup` shape but seed 9-char synthetic external ids `B11902001/B11902002` and THREE problems, and PUT the three typed id-regions via `putIDRegions` with `kind` fields). Cover, in order:

```go
func TestIDRegions_KindValidation(t *testing.T)          // duplicate kind -> 400; bad kind -> 400; roundtrip GET keeps kind+color
func TestCreateScanBatch_RequiresRegions(t *testing.T)   // no regions -> 409 regions_incomplete
func TestScanPipeline_UploadToMatrix(t *testing.T)
// upload 1 loose single-page PDF (multipart 'files'), drive the recorded
// enqueues synchronously like the old tests: SplitSource -> RenderPages ->
// stub SetScanPageIdentified? NO — wire fake.ScriptedProvider identity JSON
// (agreement) through env.scans.Providers and call IdentifyPage. Then:
// GET scan-pages shows state "assigned" with assigned_by_user=false;
// GET scan-matrix shows the cell as "auto";
// POST scan-finalize (ack_missing=true) -> 202, drive PromotePage;
// GET scan-matrix now shows "promoted";
// GET scan-pages ?state=orphan is empty.
func TestScanPages_OrphanFilterAndAssign(t *testing.T)
// illegible-name script -> orphan with proposed_student_id prefilled; POST
// assign with {student_id, problem_id} -> 200; second page to same cell -> 409
// with incumbent_page_id.
func TestScanFinalize_MissingGate(t *testing.T)          // no ack -> 409 {missing_cells}; ack -> 202
func TestScanPageCrop_KindParam(t *testing.T)            // kind=name streams jpeg; kind=bogus -> 400
func TestScanPages_NoOCRTextInBatchList(t *testing.T)    // getJSONRaw on scan-batches: response contains no "ocr_student_id" key (PII discipline, F6)
```

Use the enqueue-recording pattern on `env.scans` (`EnqueueSplit`/`EnqueueRenderPages`/`EnqueueIdentifyPages`/`EnqueuePromotePages` closures appending to slices) and drive worker bodies synchronously — the harness queue is real but unstarted.

- [ ] **Step 2: Run tests to verify they fail**

Run: `ADAMARKER_TEST_DATABASE_URL=... go test ./internal/httpapi -run 'TestIDRegions|TestCreateScanBatch|TestScanP|TestScanFinalize' -v`
Expected: FAIL — undefined handlers / 404s

- [ ] **Step 3: Implement the handlers**

Follow the shapes above; every mutation checks CSRF implicitly via the middleware (tests set `X-ADA-CSRF: 1`), extracts `me, _ := currentUser(r.Context())`, audits with IDs only. `handleCreateScanBatch` core:

```go
	files := r.MultipartForm.File["files"]
	zips := r.MultipartForm.File["zip"]
	// ... same 400 xor-validation as the old handler ...
	var sources []scan.SourceUpload
	var closers []io.Closer
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue // surfaced as skip by size-0 read in the service
		}
		closers = append(closers, f)
		sources = append(sources, scan.SourceUpload{Filename: fh.Filename, R: f})
	}
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	var zr io.Reader
	if len(zips) == 1 {
		zf, err := zips[0].Open()
		if err != nil { apiError(w, http.StatusBadRequest, "unreadable zip"); return }
		defer zf.Close()
		zr = zf
	}
	me, _ := currentUser(r.Context())
	view, err := s.scans.CreateBatch(r.Context(), aid, nb, sources, zr, me.ID)
	if errors.Is(err, scan.ErrRegionsIncomplete) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "regions_incomplete": true})
		return
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `make test-integration`
Expected: PASS — full suite, including all pre-existing packages.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/scans.go internal/httpapi/api.go internal/httpapi/scans_test.go
git commit -m "feat(api): page-level scan endpoints — batches, pages, matrix, assessment finalize"
```

---

### Task 11: Frontend foundation — types, region editor, upload, batch list

No frontend test runner exists; the gate for Tasks 11–13 is `cd frontend && npm run typecheck` plus the Task 14 manual verification. Follow the house conventions: inline `useQuery`/`useMutation` with `api.get/post/put`/`apiUpload` typed at the call site; conditional `refetchInterval` predicates (never a bare number); no toasts — inline `text-red-600` / `text-green-700` paragraphs; never console.log student data.

**Files:**
- Modify: `frontend/src/lib/types.ts`
- Rewrite: `frontend/src/components/identify/IDRegionCard.tsx`
- Create: `frontend/src/components/identify/UploadCard.tsx`, `frontend/src/components/identify/BatchListCard.tsx`

**Interfaces:**
- Produces types (append to types.ts, replacing the Task 1 stub region block):

```ts
// --- page-level scan intake (mirrors internal/httpapi/scans.go; design spec
// docs/superpowers/specs/2026-07-04-page-level-scan-intake-design.md) ----------

export type IDRegionKind = "student_id" | "name" | "problem_id";

/** Normalized 0..1 page coordinates; one region per kind, applied to EVERY page. */
export interface IDRegion {
  kind: IDRegionKind;
  x: number;
  y: number;
  w: number;
  h: number;
  color: string;
  padding: number;
}

export interface ScanBatch {
  id: number;
  assessment_id: number;
  ocr_enabled: boolean;
  ocr_provider?: string;
  ocr_model?: string;
}

export interface ScanBatchListRow extends ScanBatch {
  total_pages: number;
  processing_pages: number;
  orphan_pages: number;
  assigned_pages: number;
  parked_pages: number;
  discarded_pages: number;
  errored_pages: number;
}

export type ScanPageState =
  | "processing"
  | "orphan"
  | "assigned"
  | "parked"
  | "promoted"
  | "discarded"
  | "error";

export interface ScanPage {
  id: number;
  batch_id: number;
  page_index: number;
  state: ScanPageState;
  error?: string;
  ocr_student_id?: string;
  ocr_name?: string;
  ocr_problem?: string;
  ocr_engine?: string;
  proposal_source?: string;
  proposed_student_id?: string;
  proposed_name?: string;
  proposed_problem_id?: number;
  assigned_student_id?: string;
  assigned_name?: string;
  assigned_problem_id?: number;
  assigned_by_user: boolean;
  parked_reason?: "duplicate" | "conflict";
  parked_against?: number;
  discard_reason?: string;
  has_image: boolean;
}

export interface SkipInfo {
  filename: string;
  reason: string;
}

export interface CreateScanBatchResponse {
  batch: ScanBatch;
  created: number;
  skipped: SkipInfo[];
}

export type MatrixCellState = "empty" | "auto" | "manual" | "promoted" | "submitted";

export interface MatrixCell {
  problem_id: number;
  state: MatrixCellState;
  page_id?: number;
}

export interface MatrixRow {
  student_id: string;
  name: string;
  cells: MatrixCell[];
}

export interface ScanMatrix {
  problems: number[];
  rows: MatrixRow[];
}

export interface FinalizeReport {
  enqueued: number;
  already_promoted: number;
  missing_cells: number;
}

export interface MissingCell {
  student_id: string;
  name: string;
  problem_number: number;
}
```

- Produces components: `IDRegionCard` (three fixed-kind rects), `UploadCard` (props `{ assessmentId: string; onBatchCreated: (id: number) => void }`), `BatchListCard` (props `{ assessmentId: string }`).

- [ ] **Step 1: Rewrite `IDRegionCard.tsx`**

Keep the existing structure (RectEditor + useSamplePage + save mutation) with these changes: fixed kind colors `const KIND_COLORS: Record<IDRegionKind, string> = { student_id: "#2563eb", name: "#16a34a", problem_id: "#ea580c" };` and labels `{ student_id: "Student ID", name: "Name", problem_id: "Problem" }`. Instead of free drawing creating anonymous rects, the card shows three kind buttons; the "draw" flow stamps the selected kind + its color via RectEditor's `newRect={{ kind: activeKind, color: KIND_COLORS[activeKind], padding: 0.01 }}`; drawing a kind that already exists replaces that rect in the draft (filter + append). Rects render tinted by their kind color (`rectStyle` uses the existing `fillColor` helper). A small legend row shows which kinds are drawn / missing; the Save button disables until all three exist (`draft.length === 3`). Keep the copy-to-mask-regions button dropped — Finalize seeds masks now (say so in a caption: "Student ID and name boxes are auto-masked before AI grading."). PUT payload stays `{ regions: draft }`.

- [ ] **Step 2: Create `UploadCard.tsx`**

Simplified from the deleted version: PDFs multi-select (`accept=".pdf,application/pdf"` multiple) OR one zip (`accept=".zip"`); OCR enable checkbox + provider/model selects (fetch `["providers"]` exactly as the old card did — copy the provider-picker JSX from git history `git show HEAD~N -- frontend/src/components/identify/UploadCard.tsx` or rebuild: `api.get<{ providers: Provider[] }>("/api/providers")`, filtered to `enabled`); no problem-scope field. Submit builds `FormData` (`files` per PDF, or `zip`), `apiUpload<CreateScanBatchResponse>(`/api/assessments/${assessmentId}/scan-batches`, form)`; on success invalidate `["scan-batches", assessmentId]` + `["scan-pages", assessmentId]`, call `onBatchCreated(data.batch.id)`, render skip reasons as an inline list. A 409 with `regions_incomplete` renders "Draw the three ID regions above first."

- [ ] **Step 3: Create `BatchListCard.tsx`**

Table of batches with per-state page counts and a processing progress hint. Poll predicate:

```ts
const list = useQuery({
  queryKey: ["scan-batches", assessmentId],
  queryFn: () =>
    api.get<{ batches: ScanBatchListRow[] }>(`/api/assessments/${assessmentId}/scan-batches`),
  refetchInterval: (query) => {
    const batches = query.state.data?.batches ?? [];
    return batches.some((b) => b.processing_pages > 0) ? 2000 : false;
  },
});
```

On the processing→settled transition (useRef edge detection, same pattern as the old ReviewStrip), invalidate `["scan-pages", assessmentId]` and `["scan-matrix", assessmentId]`.

- [ ] **Step 4: Typecheck**

Run: `cd frontend && npm run typecheck`
Expected: PASS (IdentifyTab still renders only the region card + stub line; assembled in Task 13)

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/types.ts frontend/src/components/identify/
git commit -m "feat(ui): scan-page types, typed 3-region editor, page-level upload + batch list"
```

---

### Task 12: Frontend matrix + orphan queue + parked pages

**Files:**
- Create: `frontend/src/components/identify/MatrixCard.tsx`, `frontend/src/components/identify/OrphanQueue.tsx`, `frontend/src/components/identify/ParkedCard.tsx`

**Interfaces:**
- Consumes: Task 11 types; shared queries `["scan-matrix", assessmentId]` → `api.get<ScanMatrix>(…/scan-matrix)`, `["scan-pages", assessmentId]` → `api.get<{ pages: ScanPage[] }>(…/scan-pages)`, `["students"]`, `["assessment", assessmentId]` (problems list with numbers — reuse however AssessmentDetail already fetches problems).
- Produces: `MatrixCard({ assessmentId })`, `OrphanQueue({ assessmentId })`, `ParkedCard({ assessmentId })`. Shared invalidation helper per component: invalidate `["scan-pages", assessmentId]`, `["scan-matrix", assessmentId]`, `["scan-batches", assessmentId]` after any mutation.

- [ ] **Step 1: Create `MatrixCard.tsx`**

A plain `<Table>` (no virtualization — the ~250-row SubmissionsTab table is the precedent), one row per `MatrixRow`, one narrow `<TD>` per problem containing a colored dot/badge: `empty` → red-tinted "–", `auto` → indigo "A", `manual` → indigo "M", `promoted` → green "✓", `submitted` → neutral "S". Header row = problem numbers. Filters as two checkboxes: "only missing" (rows with ≥1 empty cell), "only conflicts" (rows referenced by any parked-conflict page — compute from the `["scan-pages", assessmentId]` query by matching parked pages' `parked_against` cells; keep it simple: a row filter on `pages.some(p => p.parked_reason === "conflict" && p.assigned_student_id === row.student_id)` is WRONG (parked pages aren't assigned) — instead match on the incumbent: build a Set of page ids that are parked-against, then flag rows whose cells' `page_id` is in that Set). Row/column tallies in the header line: "N students · M cells missing". Clicking a cell with a `page_id` opens a `Dialog` with `SafeImage src={`/api/scan-pages/${pageId}/image`}` and an Unassign button (`POST /api/scan-pages/{id}/unassign`, 204).

- [ ] **Step 2: Create `OrphanQueue.tsx`**

The keyboard-driven successor to the old ReviewStrip, scoped to orphans only. Reuse the deleted ReviewStrip's mechanics verbatim (they're in git history — `isFormTarget`, id-anchored cursor with `lastIdxRef`, focusable wrapper div with `tabIndex={0}`): `visible = pages.filter(p => p.state === "orphan" || p.state === "error")`. Per current page render three `SafeImage` crops (`/api/scan-pages/${id}/crop?kind=student_id|name|problem_id`, each `className="h-16 object-contain"`, labeled) + a toggleable full page (`v`), the OCR text readouts (plain text — it's PII, render only, never log), and the action panel: student picker (pre-filled from `proposed_student_id`; roster search identical to the old one: client-side filter of `["students"]`, exclude withdrawn, slice 20) + problem `<Select>` (pre-filled from `proposed_problem_id`), Assign button → `api.post(`/api/scan-pages/${id}/assign`, { student_id, problem_id })`; a 409 renders the incumbent inline ("cell already has page #N — resolve in Parked" or duplicate note). `d` opens the discard dialog (reason input), `j/k` navigate, `Enter` assigns when both pickers are filled. Footer hint: `j/k navigate · Enter assign · d discard · v toggle page`. A `proposal_source === "ocr_disagree"` page shows an amber "ID and name disagree — check both boxes" banner.

- [ ] **Step 3: Create `ParkedCard.tsx`**

Two sections from the same `["scan-pages"]` data: **Conflicts** (`parked_reason === "conflict"`) — each renders the parked page image and, when `parked_against` is set, the incumbent side by side (`grid sm:grid-cols-2`), with buttons "Keep incumbent" → `resolve-conflict {action:"keep"}` and "Replace" → `{action:"replace", force}` (force checkbox appears with the ApiError 409 text when the first attempt reports a graded guard). **Duplicates** (`parked_reason === "duplicate"`) — collapsed by default (`<details>` element), each row shows page id + incumbent id + a one-click Discard; a "Discard all duplicates" button loops the visible list sequentially.

- [ ] **Step 4: Typecheck + commit**

Run: `cd frontend && npm run typecheck` — Expected: PASS

```bash
git add frontend/src/components/identify/
git commit -m "feat(ui): assignment matrix, orphan queue, parked-page resolution"
```

---

### Task 13: Frontend finalize card + Identify tab assembly

**Files:**
- Create: `frontend/src/components/identify/FinalizeCard.tsx`
- Rewrite: `frontend/src/pages/IdentifyTab.tsx`
- Modify: `frontend/src/lib/helpContent.tsx` (new help entries)

**Interfaces:**
- Consumes: `api.get<{ missing: MissingCell[] }>(…/scan-missing)`, `api.post<FinalizeReport>(…/scan-finalize, { ack_missing })`, Task 11–12 components.

- [ ] **Step 1: Create `FinalizeCard.tsx`**

Shows: assigned-unpromoted count + missing-cell count (from `["scan-pages"]` + a `["scan-missing", assessmentId]` query, the latter `enabled` only when the ack dialog opens). Finalize button → if `missing_cells > 0` (known from a first 409 or the pages data) open the ack `Dialog` listing missing cells grouped by student (`MissingCell[]` → "B11… · Q2, Q3"), confirm → re-post with `ack_missing: true`. On 202 invalidate `["scan-pages"]`/`["scan-matrix"]`/`["scan-batches"]`; promotion progress is the matrix flipping to green under the existing poll (pages with `state === "assigned"` keep the `["scan-pages"]` query polling: `refetchInterval` predicate `pages.some(p => p.state === "processing" || p.state === "assigned") ? 2000 : false`). Structured 409 text via the old `finalizeErrorMessage` pattern (ApiError `.details.missing_cells`).

- [ ] **Step 2: Rewrite `IdentifyTab.tsx`**

```tsx
import { BatchListCard } from "../components/identify/BatchListCard";
import { FinalizeCard } from "../components/identify/FinalizeCard";
import { IDRegionCard } from "../components/identify/IDRegionCard";
import { MatrixCard } from "../components/identify/MatrixCard";
import { OrphanQueue } from "../components/identify/OrphanQueue";
import { ParkedCard } from "../components/identify/ParkedCard";
import { UploadCard } from "../components/identify/UploadCard";

export function IdentifyTab({ assessmentId }: { assessmentId: string }) {
  return (
    <div className="space-y-4">
      <IDRegionCard assessmentId={assessmentId} />
      <UploadCard assessmentId={assessmentId} onBatchCreated={() => {}} />
      <BatchListCard assessmentId={assessmentId} />
      <MatrixCard assessmentId={assessmentId} />
      <OrphanQueue assessmentId={assessmentId} />
      <ParkedCard assessmentId={assessmentId} />
      <FinalizeCard assessmentId={assessmentId} />
    </div>
  );
}
```

(`onBatchCreated` no longer selects a batch — the matrix/orphan views are assessment-wide; drop the prop if unused after wiring.)

- [ ] **Step 3: Help content**

Add to `helpContent.tsx`: `scanPageLifecycleHelp` (processing → orphan/assigned → parked → promoted; auto-assign requires ID+name agreement + valid problem), `scanParkedHelp` (duplicates vs conflicts; re-uploads only fill missing cells), `scanFinalizeHelp` (assessment-wide, incremental, ack-missing). Attach via `HelpTip` in the respective cards.

- [ ] **Step 4: Typecheck + build + commit**

Run: `cd frontend && npm run typecheck && npm run build`
Expected: PASS

```bash
git add frontend/src
git commit -m "feat(ui): assemble page-level Identify tab with assessment-wide finalize"
```

---

### Task 14: Decisions, docs, and full verification

**Files:**
- Modify: `docs/DECISIONS.md` (append D63–D68 + update the trailing "not yet decided" note)
- Modify: `docs/superpowers/specs/2026-07-04-page-level-scan-intake-design.md` (footnote the two implementation refinements)
- Modify: `frontend/src/lib/helpContent.tsx` / `README.md` only if they reference the removed flow

**Interfaces:** none — documentation + verification.

- [ ] **Step 1: Append decisions**

Append to `docs/DECISIONS.md` in the house format (`## D63 — <title> — \`v0-default\` *(2026-07-04 page-level scan intake)*`), one entry each, cross-referencing the spec:
- **D63** — the page is the staging unit; promotion through the D22 per-problem image seam; file-level flow deleted.
- **D64** — auto-assign requires independent ID+name agreement; exact-only ID rung; illegible name orphans even with a clean ID; disagreement flagged `ocr_disagree`.
- **D65** — occupied cells are never overwritten: duplicate/conflict parking, fill-only re-uploads, graded replace only via explicit force on resolve-conflict.
- **D66** — finalize seeds `student_id`+`name` id-regions into `mask_regions` (`page_scope='all'`); problem region never masked.
- **D67** — old scan staging dropped in migration 0029; in-flight batches must be drained before upgrade; id-regions must be redrawn (typed).
- **D68** — transport: sources streamed to the blob store, `MaxSourceBytes` 2 GiB (var, not config — flag if you want a knob), zip cap raised to 2 GiB, chunked rendering (25 pages/PDFium open).

Update the spec header note ("Decisions D63–D68 below get recorded in DECISIONS.md when implemented") to point at DECISIONS.md, and footnote §4/§5: `scan_sources` replaced the single-source `UNIQUE(batch_id, page_index)`; `finalized_at` dropped.

- [ ] **Step 2: Full verification sweep**

Run, in order, expecting every one green:

```bash
make test
make test-integration
make build
cd frontend && npm run typecheck && npm run build && cd ..
go vet ./...
```

- [ ] **Step 3: Manual smoke (the real workflow)**

`make run` against the dev DB, then in the browser: import a small roster CSV → create an exam with 3 problems → Identify tab → draw the three regions on a sample page → upload a small multi-page PDF (a few scanned/synthetic pages with handwritten-style headers) → watch split/render/identify progress → check the matrix, resolve an orphan, finalize, confirm promoted cells and that the Masking tab shows the seeded regions. **Do not commit any real student scans or roster data while testing.**

- [ ] **Step 4: Commit**

```bash
git add docs/DECISIONS.md docs/superpowers/specs/2026-07-04-page-level-scan-intake-design.md
git commit -m "docs: record D63-D68 page-level scan intake decisions"
```

---

## Plan self-review notes (already applied)

- **Spec coverage:** §1–3 → Tasks 2–6 semantics; §4 schema → Task 1; §5 pipeline → Tasks 4–5, 9; §6 matching/parking → Tasks 3, 6, 7; §7 UI → Tasks 11–13; §8 promotion+masking → Task 8; §9 deletions → Task 1; §10 errors/PII → woven through Tasks 5–10 taxonomies; §11 testing → each task's test list; §12 out-of-scope respected (no multi-page answers, no de-skew, Submissions tab untouched).
- **Known judgment calls an executor must not "fix" silently:** `scan_sources` vs the spec's per-batch page uniqueness (documented in the header); `finalized_at` dropped; `MaxSourceBytes` as a var; new job kind strings so retired arg shapes can't mis-decode; incumbent pages get their `submission_id` cleared on conflict-replace so derived state stays truthful.
- **Type-consistency spot checks:** `PromotePage` struct vs `EnqueuePromotePages` seam; `renderChunk` fixture type vs `EnqueueRenderPages(sourceID, pageIDs)`; `scanPageJSON.state` enum matches `ScanPageState` in types.ts; `matrixCellJSON.state` matches `MatrixCellState`; `ErrCellOccupied` JSON keys match the OrphanQueue's 409 handling.
