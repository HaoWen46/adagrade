# Workflow guards (hazard audit follow-up) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface inconsistent-state hazards (30 confirmed high-severity, audit 2026-07-10) as consistent warnings at the moment of the risky action, without changing workflow semantics — warn, don't block, except where the action is guaranteed to fail.

**Architecture:** One derived-state warnings mechanism, three surfaces. (1) `GET /api/assessments/{id}/workflow-warnings` returns structured warning codes with counts (statuses derived, never stored — house style). (2) The two action chokepoints — `GET /api/runs/preview` and the publish preview — gain a `warnings[]` array in their existing responses so hazards surface inside the Launch and Publish dialogs, scoped to the attempted action. (3) A reusable `<WorkflowNotice>` component renders every warning identically (tone + message + count + fix-it link) on tabs, Overview, and dialogs. Plus five bespoke fixes where a warning is the wrong tool (wrong email copy, misleading rubric copy, missing confirm dialogs, invisible discarded pages).

**Tech Stack:** Go 1.26 + sqlc (queries in `internal/store/queries/`, regenerate `make sqlc`), React 19 + TanStack Query 5.

## Global Constraints

- New Go logic test-first (CLAUDE.md). Integration tests: `docker compose up -d --wait db-test` then `ADAMARKER_TEST_DATABASE_URL=postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable go test ./internal/... -count=1 -run <Name>`.
- Do NOT commit/push — orchestrator commits at the end. Parallel agents share the tree; edit ONLY files your task owns.
- Never log student PII. Decimal values stay strings.
- **Do not change gating semantics**: `Publishable()` stays as-is; run creation is not newly blocked except the provider-disabled case (guaranteed failure, mirrors the mask-gate precedent).
- Frontend gate: `cd frontend && npx tsc --noEmit` (errors in other tasks' files are expected mid-flight; your own files must be clean).

## Shared contract (all tasks use EXACTLY this)

Go (new file `internal/httpapi/warnings.go`):
```go
type WorkflowWarning struct {
    Code     string `json:"code"`
    Severity string `json:"severity"` // "info" | "warning" | "danger"
    Count    int64  `json:"count,omitempty"`
    Detail   string `json:"detail,omitempty"` // machine-neutral supplement (e.g. "3 orphaned, 1 parked"); NO student names/ids
}
```
TypeScript (added to `frontend/src/lib/types.ts` by Task F1 ONLY):
```ts
export interface WorkflowWarning {
  code: string;
  severity: "info" | "warning" | "danger";
  count?: number;
  detail?: string;
}
```
Warning codes (fixed vocabulary; backend emits, frontend maps to copy+link):
`stranded_scan_pages` (orphan+parked+errored pages), `assigned_unpromoted_pages` (resolved but Finalize not clicked), `quarantined_uploads`, `batch_processing`, `mask_errors`, `superseded_answers` (graded answers whose images were force-replaced), `run_in_progress`, `no_rubric_problems`, `mixed_method_versions` (officials span >1 version of the final method), `adjusted_spot_checks` (verdict=adjusted but grade unchanged), `active_run_overlap` (launch-scoped), `provider_disabled` (launch-scoped, danger), `email_file_provider`, `email_replyto_dead` (publish-scoped), `skipped_students` (publish-scoped).

Frontend copy lives in ONE place: `frontend/src/lib/warnings.tsx` exporting `warningView(w: WorkflowWarning, assessmentId: string): { message: string; to?: string; tone: "info"|"warning"|"danger" }` — professor-friendly copy per code, `to` = the tab/page that fixes it (e.g. stranded_scan_pages → `/assessments/{id}?tab=identify`).

## File Ownership (hard boundaries)

| Task | Files |
|---|---|
| B1 warnings-endpoint | **new** `internal/httpapi/warnings.go`, **new** `internal/store/queries/warnings.sql`, regenerated `internal/store/db/*`, `internal/httpapi/api.go` (one route line), **new** `internal/httpapi/warnings_test.go` |
| B2 launch-preflight | `internal/httpapi/runs.go` + its test file |
| B3 publish-preflight + email deadline | `internal/httpapi/publish.go`, `internal/publish/publish.go` (preview struct only), `internal/publish/sender.go`, matching test files |
| F1 warnings-frontend | **new** `frontend/src/components/WorkflowNotice.tsx`, **new** `frontend/src/lib/warnings.tsx`, `frontend/src/lib/types.ts` (WorkflowWarning + preview-response additions ONLY), `frontend/src/pages/OverviewTab.tsx`, `frontend/src/pages/MaskingTab.tsx`, `frontend/src/components/identify/*` (banners + discarded card) |
| F2 dialogs-and-confirms | `frontend/src/pages/Runs.tsx`, `frontend/src/pages/PublishTab.tsx`, `frontend/src/pages/ProblemPanel.tsx`, `frontend/src/pages/AssessmentDetail.tsx` (renumber confirm), `frontend/src/pages/Assessments.tsx` (archive confirm) |
| DOC deferred-hazards | `docs/PLAN_GAPS.md` (append deferred-hazard section) |

---

### Task B1: `GET /api/assessments/{id}/workflow-warnings`

TDD. Endpoint returns `{"warnings": [WorkflowWarning...]}`, any signed-in user, route registered next to the scan routes in api.go. Compute each code with cheap COUNT queries in `warnings.sql` (mirror joins from existing queries — scan.sql:169-213 for page states, publish.sql for answers). Codes emitted here: `stranded_scan_pages` (severity warning; Detail breaks out "N orphaned, N parked, N failed"), `assigned_unpromoted_pages` (warning), `quarantined_uploads` (warning), `batch_processing` (info), `mask_errors` (danger), `superseded_answers` (warning; answers with a non-empty flags containing image_superseded AND at least one grading record — see ingest.go:375-389), `run_in_progress` (info), `no_rubric_problems` (warning; problems with no rubric version), `mixed_method_versions` (warning; only when final_source_kind='method': count distinct method_version_id over official records of that method > 1 → Count = answers on non-latest versions), `adjusted_spot_checks` (warning; spot-check verdicts 'adjusted' whose answer's official record is still the checked record — see runs.go:597-639 for the verdict tables). Empty result = `{"warnings": []}`. If a code needs disproportionate machinery, skip it and say so in your report — do not fake it. **B1 additionally owns ALL new SQL for B2/B3** (single `make sqlc` run, no concurrent regeneration): add `CountActiveRunsForAssessment` (pending/running runs) and any count queries B3's preview warnings need that don't already exist; B2/B3 consume the generated functions and run NO sqlc. Tests: seed each state via existing fixture helpers (ingestion_test.go, scans_test.go, runs tests) and assert the exact code+count; plus a clean-assessment test asserting empty.

### Task B2: launch preflight warnings

TDD. Extend `handleRunPreview` response (runs.go:52-79) with `warnings []WorkflowWarning`: `stranded_scan_pages` + `assigned_unpromoted_pages` + `quarantined_uploads` (assessment-wide; reuse B1's sqlc queries once generated — coordinate by using the same query names; if B1's queries aren't merged yet, write your own in runs.go using inline SQL via existing store patterns — DO NOT edit warnings.sql, B1 owns it), `no_rubric_problems` (scoped: only problems in the run's scope), `active_run_overlap` (a pending/running run exists for the same assessment), `provider_disabled` (danger: the chosen method's latest version names a provider that is missing or disabled — also enforce in `handleCreateRun` with a 409 apiError "method's provider is disabled"). Tests for each + the 409.

### Task B3: publish preflight warnings + honest regrade deadline

TDD. (1) Extend the publish preview response (find the struct in internal/publish/publish.go / httpapi/publish.go) with `warnings []WorkflowWarning`: `stranded_scan_pages`, `quarantined_uploads`, `run_in_progress`, `mixed_method_versions`, `adjusted_spot_checks` (same queries/logic as B1 — reuse generated queries; do not edit B1's files), `skipped_students` (info→warning when >0: count of students who will receive NO email because every problem is no_submission — publish.go:448-450 semantics), `email_file_provider` (danger in production env, info in development: provider == "file"), `email_replyto_dead` (danger: provider == "smtp" && reply domain set — regrade replies are never received, main.go:309-315 already logs this at startup; plumb the email config/env into the preview handler). (2) Fix the grade email's advertised regrade deadline (sender.go:99): the honest deadline is the EARLIER of the assessment's `regrade_deadline` (webhook-enforced) and send-time+token-window; when the assessment deadline is unset keep current behavior. Test both branches.

### Task F1: warnings frontend framework + standing banners

(1) `WorkflowNotice.tsx`: one component, `{tone, children, to?}` — amber/red/neutral banner matching the existing Totals-banner style (rounded-md px-3 py-2 text-xs ring-1 ring-inset; bg-amber-50/red-50/neutral-50), with optional Link. Replace nothing existing yet — new call sites only. (2) `lib/warnings.tsx` with `warningView` per the shared contract (copy guide: say what's wrong + consequence + where to fix; e.g. stranded_scan_pages → "N scanned pages aren't attached to any answer (…detail…). Those answers grade incomplete or not at all — resolve them on the Identify tab."). (3) react-query hook `useWorkflowWarnings(assessmentId)` on key `["workflow-warnings", assessmentId]`. (4) OverviewTab: render each warning as a compact notice line under its related step (map: stranded/assigned_unpromoted/quarantined/batch → step 2; mask_errors → step 3; run/no_rubric/mixed/adjusted → steps 4-5; email_* handled by PublishTab not Overview). (5) MaskingTab: danger notice when `mask_errors` (copy: "N pages failed masking and still carry an OLD accepted mask — the AI would see the unmasked/stale image. Re-draw regions or re-apply."). (6) Identify components: `assigned_unpromoted_pages` notice on the FinalizeCard ("Assignments take effect only after Finalize — N resolved pages are waiting"), and a new collapsed "Discarded pages (N)" card listing discarded pages via the existing `?state=discarded` pages endpoint with an Undiscard button (POST endpoint exists: scans.go:662-674 — check exact route). Also add the parked-card honesty line: "Keeping one page discards the other — the system stores one page per student per problem." (7) types.ts: add `WorkflowWarning` + extend the RunPreview and publish-preview interfaces with `warnings?: WorkflowWarning[]` — exactly these, nothing else.

### Task F2: chokepoint dialogs + confirms

(1) Runs.tsx Launch dialog: render `preview.data.warnings` via `warningView` below the existing mask-blocker block; `provider_disabled` renders red AND disables Launch (extend `maskOk`-style gating); others are amber and do not block. (2) PublishTab: render preview `warnings` prominently above the Publish button; when any warning has severity ≥ warning, the publish confirm dialog additionally requires an acknowledgment checkbox ("I understand N issues are outstanding") before the existing typed-name confirm proceeds — do not touch the backend gate. Elevate the existing "Skipped (no submission — no email)" list visually when `skipped_students` present (amber ring + one-line explanation "These students get NO email — if one of them actually submitted, their pages are stranded in intake."). (3) ProblemPanel rubric editor: fix the misleading banner copy to: "Saving creates version N. Grades recorded under older versions stop counting toward official grades until re-graded (or manually re-entered) under vN." — and when the problem has any grading records (fetch problems/summary or an existing per-problem signal; if none cheap, use ReviewTab's ProblemSummary via query), replace direct save with a confirm Dialog repeating that sentence with the live count. (4) AssessmentDetail problem-edit dialog: when the assessment has a live publish batch (assessment payload or publish preview provides this — check; else fetch publish state), confirm before saving a changed `number`: "Students' regrade emails reference problem numbers — renumbering mid-regrade-window will confuse threads. Continue?". (5) Assessments.tsx: Archive button gets a confirm dialog; content warns archive is cosmetic and lists nothing — copy: "Archiving only hides this assessment from the default list. Publish batches, regrade windows, and runs continue unaffected." (no backend data needed).

### Task DOC: deferred hazards → PLAN_GAPS.md

Append a "Deferred workflow hazards (audit 2026-07-10)" section listing, with one paragraph each (state → risk → why deferred → suggested future fix): one-page-per-cell model vs multi-sheet answers; concurrent manual grading last-write-wins; resend uses publish-time email snapshot; deactivating a user who is an assigned regrade TA; cancelled-run records eligible for officials; republish reverting adopted regrade overlays; stale seeded mask rects after id-region edit (append-only seeding); provider-disable while runs active; roster re-import email changes vs open regrade threads. Cite the audit evidence file:lines from this plan's origin (they are in the session transcript; summarize, don't fabricate).

## Verification gate (orchestrator)

1. `make vet && make test`; full `go test ./... -count=1` with test DB.
2. `cd frontend && npm run typecheck && npm run build`; `make build`.
3. Browser: workflow-warnings endpoint returns stranded-page warnings on the Demo Exam (it has 40 orphans); Launch dialog on Demo Exam shows the stranded warning; Publish preview on Smoke Exam shows skipped/file-provider warnings; Masking/Identify/Overview banners render; rubric confirm dialog; archive confirm.
4. Commit in logical groups, no push.
