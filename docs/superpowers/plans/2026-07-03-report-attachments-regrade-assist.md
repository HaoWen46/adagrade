# Report Attachments + Regrade Assist — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Normative spec: [`2026-07-03-report-attachments-regrade-assist-design.md`](../specs/2026-07-03-report-attachments-regrade-assist-design.md) — behavior questions are answered there by section number.

**Goal:** Per-student result PDFs (side-by-side page + grades) attached to grade emails with 3 quality options + ZIP fallback, individual resend, regrade escalation semantics (turn 4+ = manual), reply→problem matching, and TA-triggered stricter AI re-grades (single + batch).

**Tech:** existing stack + `github.com/go-pdf/fpdf` (new ReportRenderer seam, D42) + Noto Sans TC via `make report-fonts` (never committed).

## Global Constraints

Unchanged from the overnight plan (test-first; no PII in logs/fixtures — regrade text and page images ARE PII; money/points as decimal strings; explicit-path `git add`; no push; F17 drain semantics on new jobs; only the schema task runs `make sqlc` until it lands, then coordinated). Migrations are assigned: `0022_publish_attachments.sql`, `0023_regrade_assist.sql`. The identity-XOR-content law (D19/D51): AI-regrade prompts use masked images + redacted request text only.

---

### Task W1: schema + store (0022, 0023 + queries)
**Files:** migrations/0022_publish_attachments.sql, 0023_regrade_assist.sql; internal/store/queries/{publish,regrade}.sql; internal/store/{publish,regrade}.go(+tests); regenerated internal/store/db.
Spec §10. Backfill `turn` by verified-count ordering per (student, assessment).
**Produces:** `Store.SetRegradeProblem(id, problemID)`, `Store.SetRegradeAIRecord(id, recordID)`, `Store.ListEligibleAIRegrades(assessmentID)` (received/under_review, !escalated, ai_record_id NULL), batch attachment fields on CreatePublishBatch/Get, `Store.GetPublishItemForResend(id)` (item + batch settings).
Commit: `feat(store): publish attachment settings + regrade turn/escalation/problem/ai-record (0022-0023)`.

### Task A: internal/report + email attachment plumbing (DB-free; parallel with W1)
**Files:** Create internal/report/{report.go,layout.go,zip.go}(+tests), Makefile `report-fonts` target; Modify internal/domain/seams.go (`Attachment` + `OutboundEmail.Attachments`), internal/email/{smtp.go,file.go,postmark.go,email.go}(+tests), internal/config (ADAMARKER_REPORT_FONT), cmd/adamarker/main.go (wire font path).
Spec §3. `report.Build(ctx, ReportInput) ([]byte, error)` where ReportInput = {AssessmentName, StudentName, StudentID string; Problems []ProblemReport{Label string; Pages [][]byte; Criteria []CriterionLine; Total, Max string}; Quality "compressed"|"original"} — pages as JPEG bytes, caller fetches blobs. `report.BuildZIP(same) ([]byte, error)`. Downscale: long edge 1600px q75 via existing internal/imaging helpers where possible. PDF round-trip test uses internal/render to rasterize the output and assert non-blank halves. multipart/mixed in smtp/file builders (CRLF guard extends to filenames); postmark attachments array. Font-gated tests skip when ADAMARKER_REPORT_FONT unset; `make report-fonts` validates file size.
Commit: `feat(report): per-student result PDF/ZIP builder + email attachments`.

### Task B1: regrade escalation + problem matching (after W1)
**Files:** internal/httpapi/regrade.go(+test) (rung 5 → escalate per spec §6, HARD_MAX backstop; PATCH /api/regrades/{id} problem chip; parse heuristic on ingest), internal/config (ADAMARKER_REGRADE_HARD_MAX), new internal/regrade/match.go(+table test) for EN/ZH problem parsing (pure).
Spec §6-§7. Update existing ladder tests (turn 4 = received+escalated).
Commit: `feat(regrade): escalate-past-3 semantics + reply→problem matching`.

### Task B2: AI re-grade assist (after W1; after B1 lands to avoid regrade.go collisions)
**Files:** internal/grading/{regrade_template.go seeder bits,regrade_assist.go}(+tests), internal/queue/river.go (`regrade.ai` job on llm queue, F17), internal/httpapi/regrade.go(+test) (POST ai-regrade, POST ai-regrade-all w/ count+estimate+monthly-budget 409), redaction helper (+test: name/ID/email/token excised, counts-only logging).
Spec §8. Template kind `regrade_v1` seeded read-only; method/models pinned from the contested record (D53); result record source='regrade_ai', policy `regrade_strict`, linked via SetRegradeAIRecord. Never touches officials.
Commit: `feat(regrade): stricter AI re-grade assist (single + batch), redacted context`.

### Task C: publish wiring + individual resend (after W1 + A)
**Files:** internal/publish/{publish.go,sender.go}(+tests), internal/httpapi/publish.go(+test), internal/queue/river.go (send job builds attachment via report seam using batch settings; 15MB per-item warning surfaced on item row), POST /api/publish/items/{id}/resend (audit `publish.resend_item`), publish request gains {attachment, zip} (validated against font-configured), From display in preview payload.
Spec §3 plumbing + §4 + §2. Attachment bytes built in-job from blobs (deterministic on resend).
Commit: `feat(publish): attachment options through send pipeline + individual resend`.

### Task S-a: publish UI (after C)
**Files:** frontend/src/pages/PublishTab.tsx, lib/types.ts+api.ts (additive).
Attachment radio (3 options, disabled+hint when font unconfigured), ZIP checkbox, From display in dialog, per-row Resend button (any status), per-item size warnings.
Commit: `feat(ui): attachment options + individual resend in publish tab`.

### Task S-b: regrade UI (after B2 + S-a for types.ts serialization)
**Files:** frontend/src/pages/Regrades.tsx, AnswerView deep-link param if needed, lib/types.ts+api.ts.
Escalated badge ("manual review required", AI buttons hidden), problem chip editable + problem-scoped deep link, per-request "AI re-grade" + queue "AI re-grade all pending" with count+cost confirm, old-vs-new criteria comparison when ai_record present.
Commit: `feat(ui): regrade escalation, problem chip, AI re-grade buttons`.

### Task V: docs + gates + whole-branch review
DECISIONS D41–D53; PLAN_GAPS deltas; OPERATIONS.md + .env.adamarker.example (REPORT_FONT, REGRADE_HARD_MAX); README one-liners. Full gates; whole-branch review (most capable model) over this block; one fix wave; report.

**Order:** W1 ∥ A → B1 → B2 ∥ C → S-a → S-b → V.
