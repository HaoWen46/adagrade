# Plan 2026-07-16 — fix the publish-safety audit findings

Source of truth for findings: `docs/audits/2026-07-16-publish-safety-review-and-usability.md`
(finding IDs A1–A11, B1–B16 below refer to that document). Every finding there was
independently verified; this plan turns them into ordered, reviewable tasks.

## Global Constraints

- **TDD is mandatory** (CLAUDE.md): for every behavior change, first write a failing test
  that reproduces the finding's failure scenario, watch it fail, then fix. Tests that
  need Postgres read `ADAMARKER_TEST_DATABASE_URL` and skip when unset; the test DB is up
  at `postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable`.
- Run your package's tests both without the env var (unit) and with it (integration).
- SQL query changes: edit `internal/store/queries/*.sql`, then `go tool sqlc generate`
  (never hand-edit `internal/store/db/*.sql.go`).
- **Editing migrations 0035/0036 in place is permitted**: they have shipped nowhere except
  local dev DBs (which already applied them — code-side healing must therefore exist too
  where the plan says so). New DDL beyond that goes in a new migration file.
- Never log, commit, or paste student PII. Test fixtures use fake students only.
- Frontend: `cd frontend && npx tsc --noEmit` and `npm run build` must pass. Match
  existing component idioms (react-query, Tailwind, existing Button/Card components).
- Go: stdlib-first; match surrounding error-handling and naming style; comments state
  constraints, not narration.
- Commit at the end of your task with **explicit file paths** (`git add <paths>`), commit
  message style: `fix(area): summary` matching repo history. Do not push. Do not commit
  files outside your task's ownership list.
- Do not modify files owned by other tasks. If you believe a fix requires it, STOP and
  report BLOCKED with the reason.

## Task 1 — email package: Postmark classification, MIME header guard, note placement (A7, A11, B4-partial)

Files owned: `internal/email/**` (all files in that package).

1. **A7** `internal/email/postmark.go:162` area: today a response is
   definitely-not-accepted only when `resp.StatusCode < 500 && parsed.ErrorCode != 0`.
   Any 4xx whose JSON body lacks Postmark's `ErrorCode` (WAF/proxy/rate-limiter bodies,
   e.g. `{"error":"rate limited"}` with HTTP 429) falls through to outcome-unknown, which
   the sender quarantines as `uncertain` (manual acknowledgment per item). Fix: an HTTP
   **4xx status means the API rejected the request — the message was definitely not
   accepted**, regardless of body shape. Keep 5xx and body-parse failures on 2xx as
   uncertain (those genuinely might have been accepted). Add table-driven tests: 429 no
   ErrorCode, 403 HTML body, 422 with ErrorCode, 200 malformed JSON, 503.
2. **A11** `internal/email/mime.go:22` area: `domain.Attachment.MIME` is interpolated
   into `Content-Type: %s; name=%q` raw header lines (file/smtp builders) and into
   Postmark's ContentType field, but unlike Filename it is not covered by the CRLF
   header-injection guard. Extend the existing reject helper to validate attachment MIME
   values wherever Filename is validated today. Test: attachment with `\r\n` in MIME is
   rejected by every provider path that builds headers.
3. **B4-partial** — result emails glue the per-problem comment / regrade note onto the
   last criterion line ("…: 0/2 — Regrade turn 2: …"), so whole-problem notes read as if
   they belong to one criterion. Investigate `RenderGradeEmail` and its data shape
   (`gradeDataFromSnapshot` in internal/publish feeds it — you may NOT edit that file; if
   the fix requires changing what publish passes in, report BLOCKED with the exact change
   needed). If the template/render layer already receives a distinct per-problem comment,
   render it as its own line ("Note: …") under the problem in BOTH text and HTML templates
   instead of appending to a criterion. Snapshot-fixture tests for both templates.

## Task 2 — queue: limiter snooze gating (A8)

Files owned: `internal/queue/river.go`, `internal/queue/email_test.go` (may add a new
`internal/queue/*_test.go`).

`emailSendWorker.Work` (river.go:452) returns `river.JobSnooze(0)` for **any**
`limiter.Wait(ctx)` error. The comment's motivation (final ordinary job-timeout must not
let River discard the job while the item stays pending) is only about context-deadline
errors. A non-context limiter error (`Wait` returns an error when the wait would exceed
the context deadline OR when burst/limit config make the wait impossible) snoozes forever
at zero delay — an infinite hot loop bypassing `emailSendMaxAttempts`, invisible in the UI.
Fix: snooze(0) only when `ctx.Err() != nil` (deadline/cancel — preserves both the shutdown
drain and the final-timeout motivation). Any other limiter error returns the error so River
applies normal attempt-consuming retry/backoff. Also snooze with a small nonzero delay
(e.g. `time.Second`) instead of 0 when the context is already done, so a misconfigured
rate cannot spin the scheduler. Tests: (a) ctx-canceled wait → snooze, attempt not
consumed; (b) impossible-wait config error with live ctx → error returned (attempt
consumed).

## Task 3 — sender/delivery: timeout classification, terminal-failure return, legacy-uncertain rescue (A1, A10, A2-code)

Files owned: `internal/publish/sender.go`, `internal/publish/sender_test.go`,
`internal/domain/email_delivery.go`, `internal/domain/email_delivery_test.go`,
`internal/email/delivery.go`, `internal/email/delivery_test.go` (coordinate: Task 1 owns
other internal/email files; these two are delivery-classification files Task 1 must not
touch), `internal/store/publish.go` + `internal/store/queries/publish.sql` **only if the
rescue needs a store predicate** (Task 4 also edits these — you run FIRST; keep store edits
minimal and additive), `internal/queue/river.go` **only** for threading the job attempt
number through the `EmailSender` seam (Task 2 runs before you; rebase on its change).

1. **A1** sender.go:198: `if ctx.Err() == nil && domain.IsEmailDefinitelyNotAccepted(err)`
   — once the job's 2-minute deadline fires, even a failure the provider *proves* happened
   before any acceptance was possible (SMTP dial/EHLO/MAIL/RCPT stage) is quarantined as
   `uncertain`, requiring manual per-item acknowledgment. One slow relay during a
   200-student publish = mass manual work. Fix so that a **stage-proven**
   definitely-not-accepted classification is trusted even when ctx has expired. The
   existing comment's race concern is real for classifications that merely wrap the
   context error — so make the classification carry proof: verify (and test) that
   the SMTP provider classifies by protocol stage such that pre-DATA failures are
   definitely-not-accepted even when caused by ctx cancellation mid-dial, and that
   post-DATA/ambiguous stages are NOT so classified; then drop the `ctx.Err() == nil`
   gate in the sender. If you find a provider that wraps raw ctx errors as
   definitely-not-accepted at an ambiguous stage, fix that provider classification within
   the delivery-classification files you own (or report BLOCKED if it lives in Task 1's
   files). Tests: ctx-expired dial failure → item released for retry (non-final) /
   plain failed (final) — NOT uncertain; ctx-expired mid-DATA failure → uncertain.
2. **A10** sender.go:343: `markDeliveryFailed` returns `cause` (non-nil) even when the
   terminal DB transition succeeded, so river.go's `final && err != nil` path always
   burns one extra `emailFinalStateRetrySnooze` redelivery cycle per permanently-failed
   item. Fix: return nil when the terminal transition was durably recorded (both the
   `marked` and the benign `!marked` state-already-changed case), keep returning a joined
   error only when the DB write itself failed (that is the case the snooze mechanism
   exists for). Ensure the job then completes without the wasted cycle; adjust tests
   that asserted the old always-error contract deliberately, and say so in your report.
3. **A2-code** (heals DBs where migration 0036's backfill already flipped mid-flight
   pending items): a row with `email_status='uncertain' AND delivery_job_id IS NULL` can
   only be a 0036-backfilled legacy row — post-0036 code always sets `delivery_job_id`
   via the claim CAS before any path that marks uncertain. If the arriving River job is on
   its **first attempt** (`job.Attempt == 1`), that job never invoked a provider before,
   and pre-0036 enqueue created exactly one job per item — so the send provably never
   happened. Rescue: extend the `EmailSender` seam to pass the job attempt (or an
   `isFirstAttempt bool`), and in `SendItem`, when the loaded row matches the
   discriminator above on a first-attempt job, claim it (new store CAS:
   uncertain+NULL-job → claimed, setting delivery_job_id/generation checks as for
   pending) and proceed with the normal send path. Any other uncertain row keeps the
   current manual-acknowledgment behavior. Integration tests: (a) backfilled-shape row +
   first-attempt job → email sends, status `sent`; (b) same row + attempt 2 → untouched;
   (c) genuine post-0036 uncertain row (job id set) → untouched.

## Task 4 — publish core: empty-batch guard, coverage re-check under lock, coverage semantics, skipped count (A5, A9, B3-backend, B5-backend)

Files owned: `internal/publish/publish.go`, `internal/publish/publish_service_test.go`,
`internal/store/publish.go`, `internal/store/publish_delivery_test.go` (additive),
`internal/store/queries/publish.sql`, regenerated `internal/store/db/*`,
`internal/httpapi/publish.go` + `internal/httpapi/publish_test.go` (response shape only).

1. **A5** publish.go:557: the `len(items) == 0` guard only errors when `selectChanged`;
   a first publish (or resend-all) with zero eligible students falls through and creates
   a live 0-item batch that wedges the assessment behind ErrAlreadyPublished. Fix:
   `len(items) == 0` always returns `ErrNothingToPublish`. Test: zero-active-student
   assessment, first publish → ErrNothingToPublish, no batch row created.
2. **A9** publish.go:456 + store/publish.go CreatePublishBatch: coverage
   (Blocked/NotIngested) is only checked from an unlocked pre-read; re-verify it inside
   CreatePublishBatch's `GetAssessmentForUpdate` transaction (same queries, executed in
   the locked tx) and abort with a typed error when the gate no longer passes. Test:
   simulate an answer losing its official record between preview and create (do it
   directly against the store layer) → batch creation fails.
3. **B3-backend**: `PublishPreview.CoveragePct` (or equivalent) reads 100% while
   NOT-INGESTED students exist because not-ingested cells aren't in the denominator.
   Change the coverage computation so the denominator includes `not_ingested × problem
   count` cells (i.e. coverage = accounted / (accounted + blocked + not_ingested·problems)).
   Keep `Publishable()` semantics unchanged (still blocked>0 or not_ingested>0 ⇒ not
   publishable). Update affected tests; the JSON field the frontend reads keeps its name.
4. **B5-backend**: the batch-summary rows the batches-list endpoint returns carry
   items/sent/failed/uncertain counts; add a `skipped` count so the UI can stop implying
   lost emails (14 items / 10 sent / 0 failed / 0 uncertain with 4 invisible skips).
   Extend query + JSON + tests.

## Task 5 — grading/runs guards: zero-leaf pin, scope pin, launch preflight (A3, A4-minimal, B9-backend)

Files owned: `internal/store/grading.go`, `internal/store/grading*_test.go` (additive),
`internal/store/queries/grading.sql`, `internal/store/queries/runs.sql`, regenerated
`internal/store/db/*` (Task 4 ran before you; regenerate on top),
`internal/httpapi/assessments.go`, `internal/httpapi/runs.go`, their `*_test.go`,
`internal/httpapi/final_source_test.go`.

1. **A3**: `SetAssessmentFinalSource` accepts any `status='completed'` run — including
   runs with **zero succeeded leaves** (total failure runs), which then make
   `SpotCheckOpen` impossible (no sample exists) and wedge publish with an unreachable
   "review spot-check" call to action. Fix: reject pinning a run with zero succeeded
   grading records with a typed error surfaced by the API as 422 + machine-readable code
   + human message ("run #N graded nothing — pick a run that produced grades"). Also
   exclude such runs from whatever endpoint feeds the final-source picker (mark them
   rather than hide: include `succeeded` count so the UI can disable with a reason —
   check what the picker already receives; it currently shows "40 succeeded", so the
   count exists: just enforce server-side and document the picker rule for Task 8).
2. **A4-minimal**: same function never checks `run.ScopeKind`; pinning a problem- or
   answer-scoped run silently un-officializes every other problem (RecomputeOfficials
   joins `run_id = final_run_id` only). Decision (recorded): full supplemental-run
   layering is out of scope; the fix is to **reject pinning any run whose scope is not
   `assessment`** (typed error → 422, message explaining only assessment-wide runs can be
   the final source) and to ensure the picker data marks scope so Task 8 can disable
   those entries. Add a `docs/PLAN_GAPS.md` entry (append, own section) recording the
   deferred feature: "problem-scoped corrective runs as final-source overlays".
3. **B9-backend**: the run cost-estimate endpoint happily estimates a run that instant
   fails at launch ("method includes a reference solution but problem N has none").
   Whatever validation the run planner performs at launch, perform at estimate time too
   and return machine-readable `blockers: [{code, problem_id?, message}]` in the estimate
   response (launch keeps its own validation). Tests: estimate for a method requiring
   reference solutions against an assessment missing one → blocker present; clean
   assessment → empty.

## Task 6 — migrations hardening (A2-migration, A6)

Files owned: `migrations/0035_final_run_pin.sql`, `migrations/0036_publish_delivery_safety.sql`,
`internal/store/final_run_pin_migration_test.go`, `internal/store/publish_delivery_migration_test.go`,
`internal/store/grading.go` **only** the SetAssessmentFinalSource published-guard block
(Task 5 ran before you; rebase on its changes; coordinate carefully — your change is the
NULL-source recovery exception described below).

1. **A2-migration** (fresh-DB protection; Task 3 heals already-migrated DBs): tighten the
   0036 Up backfill so it does NOT quarantine items whose original River `email.send` job
   is still queued/available/running/retryable and has never been attempted. The
   discriminator at migration time: `river_job` rows with `kind='email.send'` whose args
   reference the item (inspect the actual args JSON shape in `internal/queue` — the
   EmailSendArgs/DeliveryRef fields — and match via `args @> …` or `(args->>…)::bigint`),
   `state IN ('available','scheduled','running','retryable')` and `attempt = 0`
   (never executed). Those rows stay `pending`; everything else keeps failing closed to
   `uncertain`. Guard for river_job's existence (migrations run before River creates its
   schema on a truly fresh DB — pending publish_items cannot exist there either, so wrap
   in a `DO $$ … IF to_regclass('river_job') IS NOT NULL` block or equivalent). Update
   the migration test: seed a pre-0036 shape with (a) a pending item with an unattempted
   queued job → stays pending; (b) a pending item with no job / an attempted job →
   uncertain.
2. **A6** 0035:33: before failing closed to NULL, add a second backfill rung: for a
   `final_source_kind='method'` assessment with no completed run for its method, pin the
   run that produced the assessment's current official model records (most-represented
   `grading_records.run_id` among answers' `official_record_id` rows with
   `source='model'`), provided that run's status is 'completed'. Only NULL out when that
   also finds nothing. Second, the recovery path: change the published-assessment guard
   (`ErrFinalSourcePublished` in store/grading.go) to **allow setting a final source when
   the current kind is NULL** (recovery from exactly this backfill, or legacy states) —
   published answers' officials are already immutable (RecomputeOfficials filters
   `published_at IS NULL`), so this cannot rewrite published grades; state that in a
   comment + prove it in a test (set source on published assessment with NULL source →
   allowed; officials of published answers unchanged; changing an already-set source
   while published still 409s). Migration tests for the new rung.

## Task 7 — roster delete (B15)

Files owned: `internal/httpapi/students.go` + `students_test.go`,
`internal/store/queries/students.sql` (or wherever roster queries live — follow the
existing file), regenerated `internal/store/db/*`, `internal/store/students*_test.go`
(additive), a new migration ONLY if an FK/index is genuinely required (prefer none).

`DELETE /api/students/{id}`, **admin-only** (requireRole admin), that hard-deletes a
roster row **only when no artifacts reference the student**: no submissions, no answers
with pages or grading records, no scan pages, no publish items, no regrade requests
(inspect the schema for the full reference list; enumerate in one query). If anything
references the student → 409 with a machine-readable body listing which artifact kinds
block deletion, and the message should point at Withdraw as the alternative. Audit-log the
deletion (`roster.delete`) like other roster actions. Tests: delete unreferenced student →
200 + row gone + audit row; delete referenced student → 409 naming the artifact kind; ta
role → 403.

## Task 8 — PublishTab frontend (B1, B3-copy, B5, B6, B13, B14, A3/A4 picker)

Files owned: `frontend/src/pages/PublishTab.tsx`, `frontend/src/lib/types.ts`,
`frontend/src/lib/helpContent.tsx`.

1. **B1**: when the publish-preview query fails with 403 (TA role), render an
   informational card ("Publishing is lecturer/admin-only — ask them to publish; you can
   still review grades on the Grading tab") instead of a blank tab. Check how the api
   client surfaces status codes; handle 403 specifically, keep generic error rendering
   for other failures.
2. **B3-copy**: the two stacked messages ("re-publish sends only to changed students…" +
   "publishing while a batch is live is refused — unpublish first") must not both render:
   when a live batch exists show ONLY the unpublish guidance; the changed-students
   sentence belongs to the state where publish is actually possible. Additionally, when
   `not_ingested > 0` AND a live batch exists, add one sentence: "The live batch does not
   cover the N unresolved students listed below." (uses the new backend coverage numbers
   from Task 4 automatically — update types.ts for any renamed/added fields).
3. **B5**: batch-history table: add a SKIPPED column fed by the new backend count; the
   columns must sum visibly (items = sent + failed + uncertain + skipped + pending).
4. **B6**: in an expanded **superseded** batch: render the "superseded — can't
   individually resend; re-publish instead" notice ONCE at the top of the expansion, and
   do not render per-row Resend buttons at all; in a live batch, hide Resend on rows with
   status `skipped` (nothing was ever sent).
5. **B13**: the Unresolved table currently stuffs guidance prose into the PROBLEM column
   per row. Move the guidance to a single line above the table; the PROBLEM cell for
   not-in-any-upload rows shows "—" (the KIND column already carries the label).
6. **B14**: "Create answer rows for N students" gets a confirmation dialog listing the
   affected students (ID + name, they're already client-side in the unresolved list) and
   stating the consequence: "students with no pages will publish as no-submission zeros
   and receive no email". Confirm button proceeds; match the existing modal idiom (see
   the archive/unpublish confirms).
7. **A3/A4 picker**: final-source dropdown: entries for runs with `succeeded == 0` or
   `scope !== 'assessment'` are disabled with a suffix ("— graded nothing" / "— problem-
   scoped; only assessment-wide runs can be pinned"). Types per Task 5's API.

After implementing: `npx tsc --noEmit` + `npm run build` must pass. State in your report
which UI states you could not exercise (the controller browser-verifies after each
frontend task).

## Task 9 — Overview/Submissions scan-path counting (B2)

Files owned: `frontend/src/pages/OverviewTab.tsx`, `frontend/src/pages/SubmissionsTab.tsx`,
plus (only if the data is genuinely absent from existing endpoints)
`internal/httpapi/ingestion.go`/`students.go` + queries + tests for one additive field.

`OverviewTab.tsx:148` counts `submission_id` only, so scan-pile assessments show
"0/14 have work" beside "56/56 published". The Submissions reconciliation already knows
per-student mapped page counts (MAPPED/EXPECTED column) — students with mapped pages but
no submission row show SUBMISSION "missing", reading as lost work. Fix both surfaces:
- Overview step 2 counts a student as having work when they have a submission OR ≥1
  mapped/promoted scan page (investigate what the students/reconciliation endpoints
  already return before adding backend fields; prefer reusing the reconciliation data).
- Reconciliation SUBMISSION cell for such students shows a "via scans" badge (neutral
  tone) instead of "missing"; header counts them ("10/14 have work · 10 via scans" or
  similar wording consistent with Overview).
- Overview's step-2 detail line and the "students joined after upload" warning must not
  contradict the new count.
`npx tsc --noEmit` + `npm run build`; list untested states for controller browser check.

## Task 10 — misc frontend (B7, B8, B10, B11, B12, B9-UI, B15-UI)

Files owned: `frontend/src/pages/Regrades.tsx`, `frontend/src/pages/RegradeRoundsTab.tsx`,
`frontend/src/components/ScoreDistribution.tsx`, `frontend/src/pages/Assessments.tsx`,
`frontend/src/pages/Runs.tsx`, `frontend/src/pages/Users.tsx`,
`frontend/src/pages/Students.tsx`, `frontend/src/lib/` (new small util file allowed).

1. **B7** Regrades drawer: rename the "Delivery details" toggle to "Reply authentication
   (SPF/DKIM)" — it shows inbound auth results and currently reads as outbound delivery
   info next to the undelivered banner.
2. **B8**: wherever users are labeled (`user #1 (admin)` fallback — find the label
   construction), fall back to the **email** when display_name is empty:
   "b11902156@ntu.edu.tw (admin)". Extract a `userLabel(u)` helper if used in >1 place
   (problems-editor TA dropdown, regrade handoff, users page).
3. **B10** RegradeRoundsTab: standardize on **turn** ("Turns" header, "Each email turn
   gets its own method…" copy — keep the domain word "round" out of the UI, or vice
   versa — pick ONE, matching the inbox which says "turn 1"); disable each turn's "Grade
   pending (N)" button when N == 0 **or** no method is selected (with a title tooltip
   "choose a method first").
4. **B11** ScoreDistribution.tsx:21: the `uppercase` class turns σ into Σ. Render the σ
   label without text-transform (`normal-case` on that StatBox or a `noTransform` prop) —
   keep the other labels uppercase.
5. **B12** Assessments.tsx:103: row navigation is onClick-only. Make the assessment name
   a real `<Link>` (react-router) so keyboard/middle-click work; keep the row onClick.
   Add `tabIndex`/`role` or rely on the Link for a11y. Apply the same pattern to other
   row-click-only tables you own in this task's file list (Runs expansion rows are
   buttons — leave those).
6. **B9-UI** Runs.tsx launch dialog: consume the estimate response's new `blockers`
   (Task 5): render them as a red list and disable Launch while any exist.
7. **B15-UI** Students.tsx: admin-only Delete button per row (next to Withdraw) wired to
   `DELETE /api/students/{id}` with a typed-confirm modal matching existing destructive
   idioms; on 409 render the returned blocking-artifact explanation and point at
   Withdraw. Hide the button for non-admins (check how the page gets the current role).
`npx tsc --noEmit` + `npm run build`; list untested states for controller browser check.

## Task 11 — tooling + docs (B16, A4 gap note, audit annotations) — controller executes

`scripts/dev-e2e.sh` rebuilds the Go binary before exec (go build is cached and fast) and
warns loudly when `internal/web/assets/dist` is missing; PLAN_GAPS entry (if Task 5 didn't
add it); annotate the audit doc findings with "fixed in <commit>" statuses; ledger wrap-up.
