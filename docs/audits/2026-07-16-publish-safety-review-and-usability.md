# Audit 2026-07-16 — commit 9115106 review + TA/lecturer usability pass

> **Status (2026-07-17, branch `fix/audit-2026-07-16`):** all findings addressed except
> three explicitly deferred remainders.
> Fixed — A1/A10/A2-code + B4-glue `41ea3bc`; A2-migration/A6 `4d11c49`+`1d98e79`;
> A3/A4-guards/B9-backend `509c4f9`+`526ade7`; A5/A9/B3-backend/B5-backend
> `47416d3`+`c50371f`+`52dc303`; A7/A11/B4-render `f71fe88`+`2f61de5`; A8 `a6396da`;
> B15-backend `3e70dab`; B1/B3-copy/B5/B6/B13/B14/A3-A4-picker `b27b0eb`; B2 `f845ae2`;
> B7/B8/B10/B11/B12/B9-UI/B15-UI `d174bba`+`ef9e0dd`; B16 with this note.
> Deferred — (1) A4 full problem-scoped-run overlay layering (PLAN_GAPS “final-source
> overlays”); (2) B4 residual: AI comments can still name internal criterion IDs —
> a prompt-template revision, needs live-model validation; (3) B8 residual:
> `/api/graders` deliberately returns no email (`TestGraders_NoEmailLeaked`), so
> unnamed staff render as “name unavailable (role)” — set display names on the Users
> page, or take a deliberate decision to expose staff emails there.

Scope: (1) adversarially-verified code review of commit `9115106` "fix: harden grading and
publish safety" (9 review dimensions, every finding independently refuted **and** reproduced
by separate agents; only findings surviving both are listed as confirmed); (2) hands-on
browser walkthrough of the running system as **admin**, **lecturer**, and **ta** across every
page, using the seeded demo data (`Demo Exam — completed`, `Midterm 2 (TA trial)`).

Baseline health: `make test` and `make test-integration` both fully green on the commit.
The delivery-ledger design itself (per-recipient status + resend, undelivered-result
surfacing in the regrade inbox, held-while-unpublished triage) held up well in testing.

Nothing here is fixed yet — this is the issue inventory.

---

## A. Confirmed code findings (verified by refute + reproduce agents)

### High

- **A1. Ordinary network timeouts are misclassified as `uncertain` (duplicate-risk) instead
  of retrying** — `internal/publish/sender.go:198`. `SendItem` only takes the
  definitely-not-accepted path when `ctx.Err() == nil`, but the SMTP/Postmark providers
  classify by protocol stage independent of ctx. A relay that blocks in `DialContext` until
  the job's deadline fires returns definitely-not-accepted (zero duplicate risk), yet
  `ctx.Err() != nil` forces `markProviderOutcomeUncertain`. One slow-relay blip during a
  200-student publish strands a batch of emails in `uncertain`, each needing manual
  per-item acknowledgment before resend.

- **A2. Migration 0036 quarantines genuinely-fresh pending emails if it runs while a batch
  is mid-flight** — `migrations/0036_publish_delivery_safety.sql:20`. The Up step flips all
  `email_status='pending'` items to `uncertain`. A restart/redeploy seconds after a publish
  (items enqueued, jobs unclaimed) reclassifies every not-yet-sent item; the queued jobs
  then no-op silently. Students never get their emails while `answers.published_at` is
  already set. One-time hazard, but it fires exactly on the deploy of this commit if any
  publish is in flight.

- **A3. A zero-leaf completed run can be pinned as final source and permanently blocks
  publish** — `frontend/src/pages/PublishTab.tsx:296` + `internal/store/grading.go`.
  A run that completes with 0 succeeded leaves never gets a spot-check sample, so
  `SpotCheckOpen` can never become true (except waive). `SetAssessmentFinalSource` only
  checks `status=='completed'`, and the picker lists such runs. The UI advertises
  "spot-check pending" with a review link that has nothing to review; the admin-waive
  escape is not reachable from that state in the UI.

- **A4. Final-run pinning breaks incremental / problem-scoped re-grading** —
  `internal/store/grading.go:44` + `queries/grading.sql:43`. `RecomputeOfficials` now joins
  strictly on `run_id = final_run_id`, and `SetAssessmentFinalSource` never checks
  `ScopeKind`, while the picker lists **every** completed run. Pinning a problem-scoped
  run (the natural move after fixing one problem's rubric) silently un-officializes every
  other problem. There is no supported way to combine "assessment run A + corrective
  problem run B" as round 0 — the cheap re-grade-one-problem workflow no longer exists.

### Medium

- **A5. First publish with zero active students creates a phantom live batch that wedges
  the assessment** — `internal/publish/publish.go:557`. The empty-items guard only fires
  for changed-only republish. With an empty/withdrawn roster the coverage gate passes
  vacuously, a 0-item live batch is inserted, and every later (real) publish 409s with
  "already published" until an admin discovers Unpublish.

- **A6. Migration 0035's backfill can strip the final source from an already-published
  assessment** — `migrations/0035_final_run_pin.sql:33`. Legacy data where a published
  assessment points at a method with no completed run backfills to NULL — the published
  assessment then shows "no source chosen (nothing is official)" while its emails are out,
  and the new `ErrFinalSourcePublished` guard blocks re-selecting.

- **A7. (plausible) Postmark classification falls through to `outcome_unknown` for
  non-Postmark JSON error bodies** — `internal/email/postmark.go:162`. A WAF/proxy/429 JSON
  body without Postmark's `ErrorCode` is neither accepted nor rejected → whole batch lands
  in manual-acknowledge territory. (Reproducer confirmed the code path; refuter considered
  the trigger unlikely in practice.)

- **A8. (plausible) `emailSendWorker` snoozes forever on limiter errors** —
  `internal/queue/river.go:452`. `limiter.Wait` failure → unconditional `JobSnooze(0)`,
  bypassing `emailSendMaxAttempts` with no backoff. Currently guarded in practice by the
  config floor on email rate; a future timeout/rate change re-opens an infinite hot loop.

### Low

- **A9. Coverage is not re-verified inside `CreatePublishBatch`'s assessment lock** —
  `internal/publish/publish.go:456`. The CAS re-checks only final-source fields; an answer
  that loses its official record between preview-read and lock still gets published from
  the stale snapshot.
- **A10. Every final-attempt terminal failure burns one extra rate-limited queue cycle** —
  `internal/publish/sender.go:343`. `markDeliveryFailed` returns non-nil even on success,
  so River always schedules one redundant redelivery pass per permanently-failed item.
- **A11. Attachment `MIME` is not covered by the CRLF header-injection guard** that
  Filename/Subject/To got — `internal/email/mime.go:22`. Safe today (hardcoded constants),
  a foot-gun the moment MIME becomes data-derived.

---

## B. Usability findings from the live walkthrough (TA + lecturer comfort)

### Blockers / trust-damaging

- **B1. TA opens Results → Publish and gets a completely blank tab.**
  `GET /api/assessments/{id}/publish/preview` 403s for role `ta` and the UI renders
  nothing — no "lecturer required" notice, tab pill still shown. A TA can't tell if the
  page is broken or forbidden.

- **B2. Scan-path assessments read as "no work collected".** On `Midterm 2 (TA trial)`
  (all intake via Identify/scan pile): Overview step 2 says **"0/14 have work"** while
  steps 3–7 say 40/40 pages accepted, run completed, **56/56 published**. Cause:
  `OverviewTab.tsx:148` counts `submission_id` only. Same on the Submissions tab:
  reconciliation header "0/14 submitted", SUBMISSION column "missing" for students whose
  MAPPED/EXPECTED is 4/4. First-contact TAs will conclude the uploads were lost.

- **B3. "COVERAGE 100%" tile sits next to "NOT INGESTED 4" (red)** on Demo Exam — and the
  coverage-gate help explicitly says publish stays disabled until the unresolved list is
  empty, yet the exam **is** published (batch predates the roster additions). Nothing
  explains "published before these students joined". Adjacent to it, "0 students changed
  since the last publish — a re-publish sends only to changed students" sits directly above
  "publishing while a batch is live is refused; Unpublish first" — contradictory guidance
  in one card, and it's unclear whether the 4 new students would even count as "changed".

- **B4. Student-facing email content leaks internals and misplaces notes** (outbox `.eml`
  inspection): an AI comment shipped verbatim with **"Strong answer for criterion 21 …
  criterion 22"** (internal rubric IDs meaningless to students); a regrade turn-2 note about
  the exchange argument was appended to the *wrong criterion line* (whole-problem notes are
  glued onto the last criterion with a dash). No sanitation/placement pass exists between
  AI comments and student emails.

### Friction / confusion

- **B5. Publish history summary omits skipped items.** Batch row reads
  `ITEMS 14 · SENT 10 · FAILED 0 · UNCERTAIN 0` — the 4 no-submission "skipped" items are
  only visible after expanding. As printed, 4 emails look lost.
- **B6. Superseded-batch ledger noise.** Every one of 14 rows shows an enabled-looking
  Resend button followed by the same repeated "Superseded — can't individually resend"
  notice; skipped (never-emailed) items get Resend buttons too.
- **B7. "Delivery details" in the regrade drawer shows inbound SPF/DKIM** of the student's
  reply — directly under a banner about the **outbound** result email failing to send.
  Mislabeled; reads as if it explains the failure.
- **B8. Regrade-TA dropdown shows "user #1 (admin)", "user #2 (lecturer)"** when users have
  no display name (the common case — the Add-user name field is optional). Impossible to
  tell who you're assigning. Same fallback presumably anywhere users are named.
- **B9. Run launcher has no pre-flight validation.** It happily launched a run that failed
  instantly with "method includes a reference solution but problem 13 has none" (runs 3, 5,
  6 in the dev DB all died this way). The failure reason is only visible after expanding
  the run row; the list just says `failed · 0/0`. The cost-estimate preflight exists — a
  validation preflight in the same place would have caught all three.
- **B10. Regrade rounds tab terminology + defaults.** "Each email turn is a round" — rows
  are labeled "turn 1/2/3" under a header "Rounds"; every round's method dropdown sits on
  "choose a method…" with an active "Grade pending (2)" button (and "Grade pending (0)"
  buttons on empty rounds).
- **B11. σ renders as Σ.** `ScoreDistribution.tsx:21` applies `uppercase` to stat labels,
  turning the std-dev σ into the summation symbol Σ on every histogram.
- **B12. Row-click tables aren't links.** Assessments list (and other tables) navigate via
  `onClick` on the row — no keyboard access, no middle/cmd-click, invisible to the
  accessibility tree. (`Assessments.tsx:103`.)
- **B13. Unresolved table misuses its PROBLEM column** for guidance prose ("not in any
  upload — add pages via Submissions or Identify"), repeated per row.

### Workflow-hazard design questions (flag, don't rush)

- **B14. "Create answer rows for N students" is one click from silent zeros.** The Publish
  tab's fix-it materializes answer rows for **all** active students (`MaterializeAnswers`
  is a CROSS JOIN, all-or-nothing); pageless students then publish as "skipped — no email"
  no-submission zeros. The stranded-pages warning banner is good, but there's no
  per-student confirmation, no explicit "mark absent" act, and no way to exclude one
  student.
- **B15. No way to remove an erroneous roster entry.** The roster is global; test/typo
  students (here: `Smoke One/Two`, `Alice`, `Bob` from old smoke tests) haunt **every**
  assessment as not-ingested blockers or no-email skip rows forever. Only Withdraw exists,
  and withdrawn students still appear in totals/exports. A delete-if-no-artifacts action
  (or per-assessment exclusion) is missing.

### Tooling note

- **B16. `scripts/dev-e2e.sh` runs the stale `./bin/adamarker`** without rebuilding — the
  binary in `bin/` predated the commit under test this session; anyone using the launch
  config after a `git pull` smoke-tests old code with no warning.

---

## C. What the commit got right (verified in testing)

Per-recipient delivery ledger with statuses + per-item resend; undelivered-result banner
and resend in the regrade drawer; "held while unpublished" triage with re-bind on
republish; role-adaptive publish copy ("ask an admin to unpublish" for lecturer); the
"4 students will receive NO email … pages stranded in intake" fail-safe banner;
final-source immutability messaging; launch-dialog deep link (`/runs?launch=1&assessment_id=`);
mask-review keyboard flow; method report card with hand-grade calibration prompt;
per-model pricing with "estimates will read unknown" hints. All unit + integration tests
pass on the commit.

## D. Suggested priority order (when fixing starts)

1. A1 + A7 (email outcome classification — the safety net currently manufactures manual work)
2. A2 (deploy-time hazard: drain the email queue before restarting onto 0036, or make the
   backfill distinguish never-attempted items)
3. A4 + A3 (final-source pinning vs problem-scoped runs; zero-leaf runs pinnable/waive dead end)
4. B1, B2, B3 (role-blank publish tab; scan-path "0/14 have work"; coverage-tile story)
5. B4 (student email content hygiene) — cheap, high trust value
6. A5, A6, B14, B15 (edge-state wedges + roster hygiene)
7. The rest of B (labels, ledger columns, launcher preflight, a11y links)
