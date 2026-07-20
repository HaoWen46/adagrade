# Roster lifecycle fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the roster survive the real NTU semester: add/drop un-enrollment, 停修 withdrawal mid-grading, retakes on the same deployment, Excel-mangled CSVs, and case/width-inconsistent student IDs — per the 2026-07-10 roster-lifecycle study.

**Architecture:** Six coordinated fixes. (1) Roster import gains a **diff**: the response reports active students missing from the CSV and withdrawn students present in it, and the Students page offers explicit bulk withdraw / reinstate from that diff — sync is proposed, never automatic. (2) Import **rejects non-UTF-8 files with an actionable message** (stdlib `utf8.Valid`; no transcoding dependency). (3) **Duplicate emails** become errors at import and a danger warning at publish. (4) **One ID-normalization regime**: scan's NFKC+casefold `NormalizeID` moves to a neutral package and becomes the fallback for filename ingest, quarantine resolve, and orphan manual assign (exact match first; unique normalized hit second; ambiguity → quarantine/orphan as today). (5) **Withdraw semantics completed downstream**: withdrawn students stop blocking publish, are excluded from new publish batches and grading runs, are refused/skipped on resend, and are visibly flagged in exports, totals, and the regrade queue — existing published history untouched. (6) A **materialize action** unblocks the late-add dead end. Plus roster codes for the existing workflow-warnings framework.

**Tech Stack:** Go 1.26 + sqlc (single owner for `internal/store/queries` + `make sqlc`), React 19 + TanStack Query 5. `golang.org/x/text/unicode/norm` is already a dependency (scan matching uses NFKC) — no new deps.

## Global Constraints

- New Go logic test-first. Integration tests: `docker compose up -d --wait db-test` then `ADAMARKER_TEST_DATABASE_URL=postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable go test ./internal/... -count=1 -run <Name>`.
- No git commands; orchestrator commits. Edit only files your task owns.
- Error messages may include student_id, NEVER name or email values (D14). Warning Detail strings: counts only.
- Decimal values stay strings. Frontend gate: `cd frontend && npx tsc --noEmit` (own files clean; other tasks' files may error mid-flight).
- **Semantics locked by user approval:** withdrawn students (a) no longer block publish via the ungraded arm, (b) are excluded from NEW publish snapshots/batches (no item, no email; already-published items and history untouched), (c) are excluded from new grading-run scopes, (d) per-item resend on a withdrawn student's item returns 409 "student is withdrawn — reinstate to resend"; resend-all/resend-failed skip them and report the skipped count, (e) remain visible in exports/totals with an explicit withdrawn marker (never silently dropped), (f) keep their regrade channel (停修 rights) but queue entries show a withdrawn flag.

## Shared contracts

- New package `internal/studentid` (created by S0): `func Normalize(id string) string` (NFKC fold → uppercase → strip non-alphanumeric; byte-for-byte the behavior of today's scan NormalizeID) and `func NormalizeName(s string) string` (moved unchanged). `internal/scan/match.go` delegates to it. Import direction: ingest → studentid, scan → studentid, httpapi → studentid; studentid imports only stdlib + x/text.
- Import response gains (additive, existing fields unchanged):
```go
type RosterDiff struct {
    MissingActive    []string `json:"missing_active"`    // active student_ids in DB, absent from CSV
    WithdrawnPresent []string `json:"withdrawn_present"` // withdrawn student_ids present in CSV (retaker trap)
    EmailChanged     int64    `json:"email_changed"`     // count only (PII)
    NameChanged      int64    `json:"name_changed"`
}
```
- New endpoints (routes registered by R1 in api.go, exact paths): `POST /api/students/bulk-withdraw` and `POST /api/students/bulk-reinstate`, body `{"student_ids": ["..."]}`, lecturer+, response `{"updated": n}`, audit-logged with counts+ids only. `POST /api/assessments/{id}/materialize-answers`, lecturer+, response `{"created": n}` (handler written by R3; R1 registers the route line verbatim: `api.HandleFunc("POST /api/assessments/{id}/materialize-answers", s.requireRole(auth.RoleLecturer, s.handleMaterializeAnswers))`).
- New workflow-warning codes (added to the fixed vocabulary; frontend copy in `lib/warnings.tsx`): `duplicate_student_names` (info; count = students sharing a name with another student; surfaced on Identify: their pages always need manual confirmation), `unmaterialized_students` (warning; active students with zero answers for this assessment while the assessment already has ≥1 answer row; fix = materialize action), `duplicate_emails` (danger; distinct emails shared by >1 ACTIVE student; publish preview + workflow-warnings).
- Grades CSV export gains a final `status` column (`active`/`withdrawn`); totals rows show a withdrawn badge. Regrade queue list responses gain `student_withdrawn bool` per item.

## File Ownership (hard boundaries)

| Task | Files |
|---|---|
| S0 foundation | **new** `internal/studentid/` (+tests), `internal/scan/match.go` (delegation only) + its test if needed, ALL of `internal/store/queries/*.sql`, regenerated `internal/store/db/*`, single `make sqlc` |
| R1 import-diff | `internal/roster/csv.go`, `internal/roster/import.go` (+tests), `internal/httpapi/students.go` (+test), `internal/httpapi/api.go` (route lines incl. R3's materialize), `frontend/src/pages/Students.tsx` |
| R2 withdraw-downstream | `internal/publish/publish.go`, `internal/publish/snapshot.go`, `internal/publish/sender.go` (+tests), `internal/httpapi/publish.go` (+test: duplicate_emails warning), `internal/httpapi/export.go` (+test), `internal/httpapi/runs.go` (consume S0's withdrawn-filtered scope queries; +test), `internal/httpapi/regrade.go` (student_withdrawn in list responses; +test) |
| R3 materialize + normalize-fallback | `internal/httpapi/ingestion.go` (+handler `handleMaterializeAnswers` + normalized quarantine lookup; +tests), `internal/ingest/ingest.go` (+test: filename normalized fallback), `internal/httpapi/scans.go` (orphan assign normalized fallback; +test) |
| R4 warnings-codes | `internal/httpapi/warnings.go` (+test): the three new codes |
| F1 frontend | `frontend/src/lib/warnings.tsx`, `frontend/src/lib/types.ts`, `frontend/src/pages/PublishTab.tsx` (materialize button on not_ingested blockers + duplicate_emails render), `frontend/src/pages/OverviewTab.tsx` (unmaterialized notice under step 2), `frontend/src/pages/IdentifyTab.tsx` or its components (duplicate-names notice), `frontend/src/pages/Regrades.tsx` + `frontend/src/pages/RegradeRoundsTab.tsx` (withdrawn badge), `frontend/src/pages/AssessmentDetail.tsx` (totals withdrawn badge) |
| DOC | `frontend/src/pages/GuidePage.tsx`, `frontend/src/lib/helpContent.tsx` (withdraw tooltip + import help updated to new reality), `docs/PLAN_GAPS.md` (mark resolved items, keep genuinely open ones) |

Note: Students.tsx belongs to R1 (import diff UI + bulk buttons + non-UTF-8 error copy). R2/R3 write NO sql — S0 provides every query.

---

### Task S0: studentid package + all SQL

- [ ] Extract `NormalizeID`/`NormalizeName` from `internal/scan/match.go` into `internal/studentid` (functions `Normalize`, `NormalizeName`) with the existing behavior EXACTLY (move the tests' expectations too; keep match.go's tests passing via delegation). Verify `go list -deps` shows no cycle.
- [ ] Queries (TDD at consumer level comes later; here `make sqlc` must compile): (a) withdrawn filters — `PublishBlockers` ungraded arm, `PublishCoverageCounts` (all arms), `PublishSnapshotInputs`, `AnswerIDsForAssessment`, `AnswerIDsForProblem` gain `AND st.withdrawn_at IS NULL` via their students join (check each join; snapshot inputs may need the join added); (b) `ExportRows` + `AssessmentStudentTotals` gain a `withdrawn` boolean column (do NOT filter); (c) new counts: `CountDuplicateActiveEmails` (active students sharing an email), `CountStudentsWithDuplicateNames` (active, sharing exact name), `CountUnmaterializedStudents(assessment_id)` (active, zero answers rows for the assessment, only when the assessment has ≥1 answers row), `ListActiveStudentIDs` + `ListWithdrawnStudentIDs` (for the import diff), `SetStudentsWithdrawnBulk(ids, withdrawn)` (single UPDATE ... WHERE student_id = ANY($1)), `MaterializeAnswers` stays as-is (already assessment-scoped; confirm signature usable standalone); (d) regrade queue list queries gain `student_withdrawn` (join students). Run the FULL existing Go test suite; the withdrawn-filter query changes may break existing publish/run tests — fix test expectations ONLY where the new approved semantics genuinely changed behavior, and report each such change.

### Task R1: import diff + bulk sync + UTF-8 + duplicate guards

- [ ] TDD parse: non-UTF-8 bytes anywhere → single error `"file is not valid UTF-8 — in Excel, use Save As → 'CSV UTF-8 (Comma delimited)'"` (line number of first offending record if available); in-file duplicate email (case-insensitive) → error naming both lines and the student_ids, never the email; two IDs equal under `studentid.Normalize` → error naming both lines/ids.
- [ ] TDD import: cross-DB duplicate email (CSV row's email equals a DIFFERENT existing student's, case-insensitive) → row error `"line N: email already belongs to student <id>"`; diff computation per the RosterDiff contract (absent-from-CSV actives; withdrawn-in-CSV; changed counts). Import stays all-or-nothing.
- [ ] TDD handlers: bulk-withdraw/bulk-reinstate endpoints (lecturer+, 403 for TA, audit-logged, `{"updated": n}`, unknown ids ignored-and-reported? — no: unknown ids make it a 400 listing them; keep it strict). Register R3's materialize route verbatim.
- [ ] Students.tsx: import result panel shows the diff — "N active students are not in this CSV" with the id list collapsed behind a toggle and a `Withdraw all N` button (confirm dialog: "These students were removed from the class list. Withdrawing keeps their history but excludes them from future grading and publishing."), "N withdrawn students are in this CSV" with `Reinstate all N` (confirm: retaker copy), email/name changed counts with a note about open regrade threads when email_changed > 0. Non-UTF-8 and duplicate errors render through the existing per-line error list.
- [ ] Import help (`?` tip) copy: mention UTF-8 requirement + Excel wording. (helpContent.tsx is DOC's file — if the tip text lives there, hand the exact copy to DOC via your report instead of editing.)

### Task R2: withdraw semantics downstream

- [ ] TDD publish: withdrawn student's ungraded-with-pages answers no longer block (S0 queries); NEW snapshots exclude withdrawn students entirely (no item, no email — extend the snapshot test matrix); already-published items untouched (assert).
- [ ] TDD resend: per-item resend for a withdrawn student → 409 `"student is withdrawn — reinstate to resend"`; resend-all/resend-failed skip withdrawn items, response gains `"skipped_withdrawn": n`.
- [ ] TDD runs: new run scopes exclude withdrawn students' answers (S0 queries); assert an in-flight run is unaffected.
- [ ] TDD publish preview: `duplicate_emails` danger warning (count from S0 query) in `warnings[]`.
- [ ] TDD export: `status` column emitted (`active`/`withdrawn`), doc comment fixed (it currently claims "one row per rostered student", which is false — make the comment honest).
- [ ] Regrade list responses gain `student_withdrawn` (S0 queries); no behavior change to the verification ladder (停修 rights preserved).

### Task R3: materialize action + one normalization regime

- [ ] TDD materialize: `handleMaterializeAnswers` runs `MaterializeAnswers` in a tx, returns `{"created": n}`; second call returns 0; lecturer+ (route from R1); audit-logged.
- [ ] TDD filename ingest fallback: exact `GetStudentByExternalID` miss → load active roster ids, match on `studentid.Normalize` equality; exactly one hit → proceed with that student (log at debug, count only); zero or >1 → quarantine as today. Test: lowercase filename vs uppercase roster matches; two normalize-colliding roster ids → quarantine.
- [ ] TDD quarantine resolve + orphan manual assign: same exact-then-normalized lookup; ambiguous → 400 "ambiguous student id". Withdraw guard stays (withdrawn student still rejected with the existing message).

### Task R4: warning codes

- [ ] TDD `workflow-warnings`: `duplicate_student_names` (info), `unmaterialized_students` (warning), `duplicate_emails` (danger) — S0 queries; clean-assessment case stays empty.

### Task F1: frontend surfaces

- [ ] `warnings.tsx`: copy + links for the three new codes (duplicate_student_names → Identify, "pages from same-named students always need manual confirmation"; unmaterialized_students → "N students joined after the last upload and have no answer rows — materialize them or upload their work", link `?tab=publish` where the button lives; duplicate_emails → Students page, "two students share an email — grade emails would go to the same mailbox").
- [ ] PublishTab: on the not_ingested blocker section, a `Create answer rows for N students` button → POST materialize, invalidate preview; render duplicate_emails danger warning.
- [ ] OverviewTab: unmaterialized_students notice under step 2 (the warnings hook already feeds Overview).
- [ ] Identify: duplicate_student_names info notice.
- [ ] Regrades + RegradeRoundsTab: amber `withdrawn` badge next to the student when `student_withdrawn`.
- [ ] TotalsCard (AssessmentDetail.tsx): withdrawn badge on withdrawn rows (S0's new column via types.ts).
- [ ] types.ts: RosterDiff on the import response, `student_withdrawn`, totals/export row `withdrawn`, materialize response.

### Task DOC

- [ ] Withdraw tooltip (helpContent.tsx) rewritten to the NEW semantics (no longer blocks publish; excluded from new batches, runs, resends; history/exports keep them with a marker; regrade channel stays open).
- [ ] Import help: UTF-8/Excel wording (take exact copy from R1's report), diff/bulk-sync explanation.
- [ ] GuidePage roster section: add the add/drop → re-import → bulk-withdraw flow and the retaker note.
- [ ] PLAN_GAPS.md: mark the roster items this plan resolves (un-enroll inexpressible → resolved via import diff; W9 email-change → partially resolved: surfaced as count) and keep genuinely open ones (true delete for typo'd rows, semester archive/reset, Big5 transcoding).

## Verification gate (orchestrator)

1. `make vet && make test`; full suite with test DB.
2. `cd frontend && npm run typecheck && npm run build`; `make build`.
3. Browser: import a CSV of existing students minus a few → diff panel renders (no mutation until clicked); non-UTF-8 file → actionable error; workflow-warnings shows new codes where seeded; materialize button on a not_ingested blocker; export CSV has status column.
4. Commit in logical groups; no push.
