# What was missing in the plan

> **Update 2026-07-02 (overnight build, phases 0–4):** many items below are now
> resolved in code — each as a **flagged v0 default documented in
> [`DECISIONS.md`](DECISIONS.md)** (D1–D15), not a silent choice. Addressed:
> A1–A9 (all Phase-0 code questions) · B-C6/B-H13 answer identity + multi-page (D1)
> · B-H1 status enums → derived status (D2) · B-C2 aggregation v0 (D3) · B-C8/B-M10
> points/total authority (D4) · B-H18/B-H20/B-L1 version pinning incl. refsols (D5)
> · B-H3/B-H16/B-M2 official-pointer locks + optimistic concurrency (D6) · B-H6
> OAuth state/PKCE/nonce (D7) · B-L2 bootstrap lockout + dev login (D8) · B-C4/B-H17
> *partially*: per-page mask review + hard run gate (D10; OCR detection still open)
> · B-M4 masked-only invariant as a type (D10) · B-H4/B-M7/B-M8 leaf states,
> retry-failed, malformed→no-record (D12) · B-C9 bulk accept-official · B-H9/D13
> roster contract + quarantine, extended by the OCR-based identification ladder
> (D21) · B-H12 partial: IDs-only logging convention (D14)
> · B-L3 down-migrations tested + db-dump order (D15) · B-C11 CSV export · B-H8
> now fully addressed: withdrawn students (D23) + missing-student handling at
> scan finalize (D18) · B-H17 partially: ingestion-reconciliation view for scan
> batches lands (D18), flag-triage queue still open · spec §13's "exact submission
> format and collection method" now pinned by D18–D23 (scan intake staging, ID
> regions, OCR matching, image/per-problem submissions, withdrawn students).
> **Still open** (phases 5–8 + ops, as of 2026-07-02): B-C1 backups, B-C3 publish state
> machine,
> B-C5 acceptance gate, B-C7 agreement semantics, B-C10 transcription PII scrub,
> B-H2 partial (temp pinned; model-retirement policy open), B-H5 cost caps, B-H7
> retention, B-H10,
> B-H11, B-H14, B-H15, B-H19, B-M1, B-M3 (superseded chain kept, GC open), B-M5,
> B-M6, B-M9, B-M11–M21.

> **Update 2026-07-03 (overnight build, Phase 6–7 + trust/cost + ops):** more items
> below are now resolved, flagged as **D28–D40 in [`DECISIONS.md`](DECISIONS.md)**.
> Addressed: **B-C3** publish state machine — named states, granularity (per-assessment),
> coverage gate, lock, unpublish, changed-only re-publish (D28–D30), hardened same-day
> with a single-live-batch guard and a fail-closed `not_ingested` blocker for roster
> students added after ingest · **B-C5** ground-truth/spot-check acceptance gate before
> bulk accept-official, deterministic stratified sample, admin waive, migration backfill
> for pre-existing runs (D37), hardened same-day so the first-drawn sample is canonical
> across retry-driven run re-completion · **B-H5** cost caps — per-model pricing feeding
> `cost_usd` at insert, a per-run cap and a monthly global cap with pre-flight estimates,
> both fail-open when unconfigured (D35–D36) · **B-H10** regrade token + inbound
> verification ladder — HMAC/HKDF token off the existing D16 master key, sender-must-match
> **current** roster email (resolves the roster-email-change question), a 5-rung ladder
> with a verified-request rate cap (D32–D33); **SPF/DKIM enforcement is v0
> warn-not-block, not hard-block** — flagged for review, university forwarders break
> strict enforcement; hardened same-day with MessageID idempotency against Postmark's
> webhook retries (migration 0020) · **B-H14** whole-cohort systematic-misread detection
> — per-criterion/total score-distribution histograms (official, falling back to AI
> grades when officials are sparse, labeled) in ReviewTab and the publish preview (D38)
> · **audit-read gap** (B-H12's write side already existed) — `GET /api/audit` +
> an Audit section on the Users page (D39) · Phase 8 **subset**: override-rate per method
> and cost-per-run reports (D40; cross-exam comparisons remain deferred, see below)
> · **B-C1 partial→mostly addressed**: nightly backup automation now exists (systemd
> timer, `deploy/backup.sh`, blobs-then-DB ordering) with a documented restore +
> ref-integrity procedure (`docs/OPERATIONS.md` §4–5, `adamarker -verify-blobs`) — an
> off-host copy of the backup is still a manual `rsync`/cron the operator sets up
> themselves, not automated by the app.
> **Explicitly still open** (nothing below was attempted tonight): bounce/complaint
> webhook handling (regrade spec §10), retention/erasure **B-H7**, the deferred batch
> API threshold (**D11**'s `execution_mode`/**B-M21**), cross-exam reports (Phase 8
> remainder beyond D40), and a new same-day finding — **partial-cohort `not_ingested`
> over-blocking**: a single roster student added mid-term with no submission expected
> blocks publish for the *entire* assessment, with no per-student exemption path (see
> DECISIONS.md's closing note).

> **Update 2026-07-03 (morning round, report attachments + regrade assist):** flagged as
> **D41–D53 in [`DECISIONS.md`](DECISIONS.md)**. Addressed: **B-M12** student-facing
> email content — a per-student result PDF (or ZIP fallback) now carries the original
> page image alongside the grading breakdown, three attachment quality options,
> plumbed through `multipart/mixed`/Postmark (D42–D45) · **B-H15** appeals fairness — the
> regrade turn ladder now escalates past `ADAMARKER_REGRADE_MAX` to a visible manual-review
> state (`escalated=true`) instead of going straight to rejection, with a new
> `ADAMARKER_REGRADE_HARD_MAX` backstop (D48) · a stricter AI re-grade assist as a
> TA-triggered second opinion — masked images, redacted request text, never auto-official,
> its own `regrade_ai` record source/`regrade_strict` policy, with a batch dry-run cost
> estimate against the D36 monthly budget gate (D50–D53) · reply→problem matching
> (EN/ZH numerals) as a TA-editable guess, not an autonomous action (D49) · individual
> per-student resend for "student says they never got it" (D46).
> **Explicitly still open** (surfaced during this round, not attempted): the
> `ai-regrade-all` dry-run estimate is scoped to a single assessment only — there is no
> cross-assessment or global cost preview; `PolicyBadge` (frontend) has no tone/label for
> the new `regrade_strict` policy value (separately patched this round, see the
> PolicyBadge commit — tracked here since it was a docs-round finding, not a planned
> task); a **turn-assignment TOCTOU**: `turn = 1 + prior verified count` is computed
> read-then-write, not inside a single guarded statement, so two genuinely concurrent
> verified replies for the same (student, assessment) could race onto the same turn
> number; a **resend double-send window**: `ResendItem` (D46) and the pre-existing
> `ResendFailed` both flip an item to `pending` and enqueue without a River
> `UniqueOpts` de-dupe key, so a TA clicking per-item resend while a batch-level
> "resend failed" is in flight (or a double-click) can enqueue two send jobs for the same
> item; **fpdf multi-image determinism** is now fully fixed (the earlier
> `/CreationDate` + `SetCatalogSort(true)` pinning did NOT cover the same-pixel-width
> case — `putimages()` sorts images by width with no tie-break, an upstream defect
> unfixed through fpdf v0.12.0; resolved in `internal/report/layout.go` by a
> content-preserving sub-integer nudge to each image's float sort key plus a `/ModDate`
> pin, guarded by `TestBuild_DeterministicWithSameWidthPages`), recorded here only for
> completeness, not as still-open; and **full-width-digit problem parsing**
> — `internal/regrade/match.go`'s `zh_num` pattern matches only ASCII digits after a
> Chinese marker (`第3題`), so a reply typed with full-width digits (`第３題`, U+FF10–FF19,
> common on CJK IMEs) fails to parse and falls back to no guess.

> **Update 2026-07-03 (N3 fix wave, second whole-branch review):** three Important seam
> defects on the D41–D53 branch fixed (see DECISIONS.md "N3 fix wave"). **I1** — individual
> resend on a **superseded** batch was a silent no-op that downgraded a `sent` item to
> `pending` and wedged it there; now 409'd (resend is live-batch-only, D46 narrowed). **I2** —
> the **single-request** AI re-grade bypassed the D52/OPERATIONS.md monthly budget gate that
> `ai-regrade-all` enforces (with `?rerun=1` re-spending indefinitely); the gate is now one
> shared helper both handlers call. **I3** — a **D51 privacy-law gap**: the contested record's
> `OriginalComment` and score rationales reached the provider **unredacted** (a HUMAN/TA-edit
> comment can name the student); now redacted with the same identity as the request body.
> Plus four minors (migration-0022 zip comment vs D45, a doubled "AI unavailable —" UI prefix,
> two "lecturer+"→"TA+" doc comments, and an `ai_error` on the no-method-pinned skip path so the
> TA's button no longer appears to do nothing). **Explicitly left as still-open** (reviewer-triaged
> acceptable, unchanged this wave): the turn-assignment TOCTOU, the resend double-send window, SMTP
> 4xx/5xx classification, full-width-digit reply parsing, poll freshness, the multi-answer mid-loop
> retry duplicate (bounded ×3), and `ai-regrade-all` fail-open on unpriced models.

> **Update 2026-07-03 (evening round, regrade v2 — D54–D62):** the regrade flow was
> rebuilt around a strict `<pN>` multi-problem format, a single-use per-turn token
> chain, per-problem verdicts with a hard-gated TA-clicked result send, and a
> final-turn TA handoff (migration 0025). **The turn-assignment TOCTOU flagged in the
> prior round's update (above: "two genuinely concurrent verified replies for the
> same (student, assessment) could race onto the same turn number") is now
> STRUCTURALLY FIXED, not just mitigated.** The old `turn = 1 + prior verified count`
> read-then-write is gone; v2 tokens carry their own turn (minted at send time,
> verified by recomputation), and a token is consumed by a **partial unique index**
> `regrade_requests(publish_item_id, turn) WHERE kind = 'filed'` (migration
> `0025_regrade_v2.sql`) enforced by Postgres — a losing concurrent filer hits a
> `23505` and is recorded as an `addendum` row instead, never a duplicate/racing
> turn. See `internal/store/regrade.go`'s `FileRegradeRequestV2` and DECISIONS.md D57.
> Also retired as part of the same round: the `HARD_MAX`/`escalated` manual-review
> band (D48, superseded by per-problem TA handoff), the v1 prose-guessing inbound
> heuristic (`internal/regrade/match.go` — left in the tree but unreferenced by the
> webhook path, D55), and the v1 single-outcome `POST /api/regrades/{id}/resolve`
> endpoint (replaced by the per-sub-item verdict + gated `send-result`).
> **Still open, not attempted this round:** per-assessment turn budgets — `MAX` is
> still one process-global `ADAMARKER_REGRADE_MAX`, not configurable per assessment
> (spec §11 "out of scope", recorded deliberately, not a bug); a **reminder
> date-anchor approximation** for turn>1 — `handleRemindRegrade`
> (`internal/httpapi/regrade.go:1239-1256`) anchors the reminder's "sent date" to
> `publish_items.sent_at`, which is only ever set once, at the ORIGINAL grade
> email's send time (there is no per-result-email send timestamp column); the
> subject line it quotes is exact for the live token's turn, but for a turn>1
> reminder the displayed date can predate when result email #(N-1) actually went
> out — the code documents this explicitly as a known approximation, not a bug; and
> a **handoff-badge N+1 UI fetch** — `Regrades.tsx`'s `HandoffBadge` (per the
> component's own comment, queue rows carry no per-problem data) issues one
> `GET /api/regrades/{id}` per `handed_off` row on queue load to resolve which TA it
> was handed to, on top of the already-batched one-per-assessment
> `GET .../ta-assignments` call; unoptimized but correct (results are cached under
> the same key the detail pane reuses), flagged for a future batching pass.

> **Update 2026-07-03 (whole-branch review fix wave — F1–F6):** (a) **the escape-hatch
> add-problem UI is deferred** — the backend endpoint (`POST /api/regrades/{id}/problems`)
> and its type exist and are exercised by tests, but no control in the regrade detail UI
> calls it yet; a TA can only add a missed sub-item today via a direct API call, not
> through the app. (b) **D60's publish-preview unassigned-TA warning is now
> IMPLEMENTED** (backend: `unassigned_ta_problems` on the publish preview response,
> `internal/publish.Preview`; frontend PublishTab warning banner is a separate,
> immediately-following pass) — previously spec'd but not built; closed this round.
> (c) `regrade_requests.status = 'under_review'` is a **never-set state** in the current
> code path (the intermediate transition it names has no production caller since the
> v1 revert path was removed) — harmless vocabulary residue in the CHECK constraint and
> status ladder comments, not a bug; left as-is rather than narrowing the CHECK, since a
> future manual "mark under review" affordance is a plausible small addition, not a
> reason to migrate the column today. Also this round: **F1** send-failure recovery
> — `regrade_requests.result_sent_at` (migration 0026) + `POST
> /api/regrades/{id}/resend-result` closes the "atomic flip-before-send resolves the
> request but the provider send fails, stranding the student with no result and no
> retry" dead end (D59's flip-before-send ordering itself is unchanged — kept as the
> correct race arbiter). **F5** folds the handoff's filed-insert + flip-to-handed_off
> into one transaction (`FileAndHandOffRegradeRequestV2`), closing a narrow window
> where a failure between the two steps could leave a live `kind='filed'` row sitting
> at turn `MAX+1` for `handleSendResult` to mistake for an ordinary adjudicable
> request. **F6** deleted `internal/regrade/match.go` (fully dead: its last claimed
> caller, the v1 `PATCH /api/regrades/{id} {problem_id}` chip, was itself removed this
> round) — the **full-width-digit problem parsing** gap noted above (a `match.go`
> limitation) is now moot, not open.

> **Update 2026-07-04 (page-level scan intake — D63–D68):** the scan-intake flow was
> rebuilt page-native (spec
> [`2026-07-04-page-level-scan-intake-design.md`](superpowers/specs/2026-07-04-page-level-scan-intake-design.md),
> flagged as **D63–D68 in [`DECISIONS.md`](DECISIONS.md)**), replacing the file-level
> `scan_files` model entirely. **B-H17 moves further from partial to mostly
> addressed**: the new assessment-wide student × problem assignment matrix (§7 item 3)
> is the cross-problem, filterable (missing/conflict) reconciliation view B-H17 asked
> for, and D66's finalize-time mask seeding masks identity on every page automatically
> rather than relying on one per-assessment preview sample. **Still open from B-H17**:
> a fast keyboard-navigable masked-crop accept/flag review, and a modeled flag
> lifecycle (open → resolved/dismissed, by whom) — nothing here reviews individual
> masked crops or tracks flag resolution state. No other open PLAN_GAPS item is
> resolved by this round (B-H7 retention/erasure is explicitly out of scope per the
> new spec's §12; scan staging data itself remains unaddressed by any retention
> policy).

> **Update 2026-07-04 (final-review fix wave):** six review findings on the page-level
> scan intake branch were fixed (finalize-button deadlock after round-1 promotion;
> local-OCR partial reads stuck in "processing" forever when cloud OCR is off, same bug
> class as the earlier no-local-reader fix; submission-backed parked conflicts and
> non-guard retract failures both leaking as 500s instead of 400/honest guidance; stale
> UI copy in the orphan queue and parked-conflict cards). One deliberate scope note from
> that pass: the design spec's §7 item 1 promised **re-identify unresolved pages after
> region edits** — re-cropping and re-running identify for orphans when the three ID
> regions are redrawn post-OCR. That was **not implemented** this round or any prior one;
> it remains deferred (regions are expected to be final before upload, per D66's
> append-only masking stance). What exists instead is a narrower, unrelated capability
> added this round: a per-page **Retry** button in the orphan queue that re-enqueues an
> *errored* page's next stage (render or identify) — error recovery, not a region-edit
> trigger. See the design spec's §7 footnote for the full note.

> **Update 2026-07-10 (roster lifecycle — import diff + withdraw downstream):** the
> 2026-07-10 roster-lifecycle study's items land per
> [`superpowers/plans/2026-07-10-roster-lifecycle.md`](superpowers/plans/2026-07-10-roster-lifecycle.md).
> Addressed: **un-enrollment was inexpressible** (import is upsert-only, so a dropped
> student could never be removed by re-importing the registrar list) → resolved via the
> **import diff**: the import response now reports active students missing from the CSV
> and withdrawn students present in it (the retaker trap), and the Students page offers
> explicit bulk withdraw / reinstate from that diff — sync is proposed, never automatic ·
> **withdraw semantics completed downstream** (user-approved): withdrawn students no
> longer block publish via the ungraded arm, are excluded from NEW publish
> snapshots/batches and new grading-run scopes (already-published items and history
> untouched), are refused on per-item resend (409; resend-all/resend-failed skip and
> report the count), stay visible in exports/totals with an explicit `withdrawn` marker,
> and keep their regrade channel (停修 rights) with a withdrawn flag on queue entries ·
> **B-H9's CSV contract hardened**: non-UTF-8 files are rejected with an actionable
> Excel "Save As → CSV UTF-8" message (no transcoding); duplicate emails are an error at
> import (in-file and cross-DB) and a danger warning at publish · **one ID-normalization
> regime**: scan matching's NFKC+casefold normalize moved to `internal/studentid` and now
> backs filename ingest, quarantine resolve, and orphan manual assign as an
> exact-match-first / unique-normalized-hit-second fallback (ambiguity still quarantines)
> · the **late-add `not_ingested` dead end** (flagged 2026-07-03 as "partial-cohort
> not_ingested over-blocking") gets a materialize action + `unmaterialized_students`
> warning, so a student added after uploads can be given answer rows without a re-upload
> · **W9 email-change → partially resolved**: re-import now surfaces `email_changed` as
> a count (PII-safe) with a Students-page note about open regrade threads — the
> deliberate per-student notify/re-issue path (and W3's live-address resend) remains
> open, see W9's status note below.
> **Explicitly still open** (roster items deliberately not attempted): **true delete for
> a typo'd roster row** — withdraw is the only removal, so a row created with a mistyped
> student_id lives in the roster forever (and its normalized form can shadow the
> corrected id in the fallback matcher); **semester archive/reset** — the roster is one
> global list across semesters on the same deployment, with no per-term scoping or
> end-of-term reset; **Big5 (or other legacy encoding) transcoding** — import rejects
> non-UTF-8 with instructions rather than converting, a deliberate stdlib-only choice.

*Generated 2026-07-01, two ways: **Part A** — gaps that surfaced from actually building the
Phase-0 skeleton; **Part B** — a six-lens adversarial audit of `ADA-Marker_Plan.md` + the
architecture spec. They overlap, which is a good sign: hands-on and analysis converge.*

## The headline

The plan is strong on **why/what** and the **shape of the domain**. What it under-specifies, in
order of pain:

1. **The grade lifecycle & aggregation core.** There is no assessment-level total, no
   defined `official`/`published`/`finalized` state machine, and no enumerated status values —
   yet emails, export, list badges, locks, and regrades all depend on it.
2. **Privacy enforced end-to-end.** Masking is one fixed rectangle + one per-assessment preview;
   there's no per-answer QA gate, and the verbatim transcription re-introduces identity into
   immutable storage, email, logs, and the queue.
3. **Trust in the AI grades.** No ground-truth/spot-check acceptance gate, undefined "agreement"
   semantics, unpinned temperature, no cohort-anomaly detection.
4. **Single-VM ops.** No backup/restore or Postgres↔blob consistency protocol, no cost caps, no
   monitoring, no disk sizing.
5. **Operator throughput.** The tool exists to grade faster, but there's no bulk "accept AI
   grades", no "re-run failed leaves only", no multi-TA concurrency control, no grade export.

**Sequencing note:** none of this blocks the Phase-0 skeleton. But the grade lifecycle,
assessment aggregation, answer-identity-on-reingest, and masking QA gate should be resolved
*before* their phases (2–6), not during. I recommend a short "domain lifecycle" decision pass
before Phase 1.

---

## Part A — surfaced by building the skeleton

These came out of writing the actual code, not analyzing the docs. Several are echoed (with IDs)
in Part B; the code carries `QUESTION:` comments pointing here.

**A1 — OAuth authorization is a cluster of undecided sub-rules.** Writing `auth.Authorize`
(`internal/auth/authorize.go`) forced choices "Google Workspace + allowlist" left open, which I
made as flagged v0 defaults:
- an allowlisted **personal-Gmail** account (no `hd`) → **allowed** (allowlist is authoritative);
- an allowlisted address arriving via a **different Workspace tenant** (`hd` mismatch) → **denied**;
- email canonicalization is **lowercase+trim only** — deliberately *not* dot/tag-stripping,
  because on a custom domain like `ntu.edu.tw` the local part is opaque.
Each needs your confirmation. (Distinct from the transport-level CSRF/state/PKCE gap, B-H6.)

**A2 — Config & secrets are entirely unenumerated.** Writing `internal/config` forced "what must
exist to boot?". I made dev/prod asymmetric (production requires a DB URL) and gave secrets **no
defaults**, but the plan never lists them: session key, regrade-token HMAC secret, OAuth
client id/secret, per-provider API keys, Postmark key, inbound domain. (→ B-H11.)

**A3 — Renderer seam: who splits vs who renders?** The spec says split→pdfcpu, render→go-pdfium,
but the `Renderer` interface can't tell whether it takes a whole PDF or one page. Boundary
undecided (`internal/domain/seams.go`).

**A4 — BlobStore→browser delivery is undefined.** Local disk has no signed URLs, so answer images
must stream through an authenticated handler — which forces the "who can see whose data" question
(TA scoping, B-M15).

**A5 — VisionProvider: where does confidence/legibility live?** Transcribe-then-grade emits
confidence *inside* the JSON, but flagging logic needs it *outside*. Typed field vs
parse-from-structured is undecided. (→ B-C4/B-C5.)

**A6 — BatchVisionProvider shape is provider-divergent.** Batch as a provider capability vs a
Queue job type is unresolved; submit/poll/expiry/partial differ per provider, and the sync
fallback for providers lacking batch is undefined. (→ B-H4.)

**A7 — EmailProvider.ParseInbound leaks provider shape.** The seam must decode Postmark's specific
JSON; token extraction (MailboxHash vs OriginalRecipient fallback) + verdict-header mapping need a
defined contract. (→ B-H10.)

**A8 — Frontend / go:embed build wiring.** `go:embed` can't reach `frontend/dist` (a sibling of
the web package); `make frontend` is currently a stub. Needs a copy step into
`internal/web/assets` or an embed package under `frontend/`.

**A9 — Health check conflates liveness and readiness.** `/healthz` only says "process up"; there's
no readiness probe (DB reachable? providers configured?). (→ B-M6 monitoring.)

---

## Part B — six-lens plan audit

*Lenses: domain/data · security/privacy · ops/reliability/cost · grading correctness/fairness · UX/throughput · requirements/contradictions. 55 gaps, ranked; IDs are referenced from Part A.*

### Cross-cutting themes

- Missing grade lifecycle & aggregation core: no assessment-level total, no defined publish/official/finalized state machine, and un-enumerated status enums — the product's central state model is hand-wavy, and nearly every downstream feature (emails, export, badges, locks, regrades) inherits the ambiguity.
- Re-ingest / Answer identity / immutability collision: re-upload, blank/absent students, multi-page answers, and rubric re-versioning all break the 'immutable records + reproducible grades' premise because Answer identity, image versioning, and record provenance on replacement are undefined.
- Privacy masking is asserted but unenforced end-to-end: one fixed rectangle + one per-assessment preview, no per-answer QA gate, no type-level guarantee, verbatim transcription re-introduces PII into immutable storage and outbound email, and PII spreads into logs/River/email-provider with no retention/erasure story.
- Grading correctness & fairness lacks validation and precise rules: no ground-truth/spot-check acceptance gate, undefined agreement semantics, unpinned temperature undermining reproducibility, no cohort-anomaly detection, and unspecified partial-credit granularity/total authority — auto-accepted grades ship to students with no measured trust.
- Single-VM operations are unhardened: no backup/restore or PG↔blob consistency protocol, no cost caps, no monitoring/alerting, no disk sizing, and under-specified batch/failure/restart recovery — routine failures at 1–3k calls/run have no defined remediation.
- High-volume operator workflow gaps: no bulk 'accept AI grades as official', no 're-run failed leaves only', no concurrency control for multiple TAs, no flag-triage queue, and no grade export — the throughput win the tool exists for is largely unbuilt between ingest and published grades.

### Contradictions & dangling decisions (plan vs spec)

1. Answer parentage: Plan §3 shape draws Student 1—* Submission 1—* Answer (Answer belongs to a Submission), but Spec §4 `answers` has NO submission_id and keys directly on (student, problem). This must be reconciled before re-upload/retention/multi-PDF behavior can be defined.
2. Rubric-version ownership: Plan §3 says the GradingMethod 'includes the rubric version' (method owns it), but Spec §4 pins rubric_version on BOTH grading_runs AND grading_records. Three pin-sites with no stated authority; if a method pins v2 but the rubric advanced to v3 at launch, which wins is undefined — and reproducibility (Plan §10) collapses if ambiguous.
3. Batch-vs-sync threshold: Spec §1 decides 'Batch by default for multi-answer runs; sync for single-answer/interactive' and §6 says Batch 'when the set exceeds a threshold', but the threshold NUMBER is never given (2? 10? 50?). Dangling: batch's multi-hour latency vs sync's interactivity hinges on an unbound value. (Plan §1 flagged it; spec left it open.)
4. Regrade default action: Both Plan §12 and Spec §13 list 'auto re-grade vs straight-to-human' as an OPEN question, yet Phase 7 (Plan §11 / Spec §11) is specced to be built with that central branch admittedly undecided. Re-running the SAME method on the SAME masked image likely reproduces the SAME grade, so 'auto re-grade' may be near-useless unless it escalates to a different model — undecided.
5. 'Never silently overwrite a human-finalized/official grade' (Plan §10 / Spec §10) vs Spec §6 step 4 Reconcile 'auto-set[s] official grade on agreement' with no check that the answer was human-finalized. The guardrail exists in prose; the auto-set mechanism has no enforcing precedence rule — a direct plan-vs-spec conflict the moment problem-scope runs and manual grading coexist.
6. Immutability vs erasure: Spec §4 declares grading_records 'append-only, immutable' and 'kept forever'; Plan §10 pushes archive/soft-delete + forever history. Neither reconciles this with any FERPA-like right-to-be-forgotten / retention obligation for education records containing name+ID+email — the safety mechanism structurally prevents deletion, and the tension is never acknowledged.
7. Calibration: Both Plan §12 and Spec §13 list 'whether assignments calibrate methods before exams' as open, but the data model has no hook (no method-quality metric, no method↔assessment performance linkage), so even if answered later there is nowhere to record or act on it — a dangling decision with no schema affordance.
8. Model retirement: Plan §12 flags provider retiring a pinned model id mid-term; neither doc resolves what grades a regrade for an answer originally graded on a now-dead model, or whether the 'same method' silently changes across an assessment. Left dangling.

### Critical (11)

**B-C1 — No backup/restore strategy, and Postgres↔blob-store consistency is unhandled**  
*Operations & reliability*  
Spec §2 puts Postgres and blobs on one VM's local disk with no backup and no consistency protocol between the two independent stores. grading_records/submissions/answers hold only refs (source_pdf_ref, image_ref, masked_image_ref) into the disk store. A disk loss or corruption destroys a term of grades irrecoverably; even with backups, a pg_dump at T1 and a disk backup at T2 restore rows pointing at missing files or files with no owning row.  
*Fix:* Specify: nightly pg_dump + WAL archiving (or pg_basebackup) to a second host; a snapshot ordering making PG the source of truth (back up blobs first, then dump PG; disk-only files are reclaimable, PG refs with no file are surfaced restore errors) OR content-addressed write-once blobs so PG point-in-time restore never dangles; a documented restore + ref-integrity verification procedure with target RPO/RTO.

**B-C2 — No assessment-level total exists in the model; per-answer 'total' is the only grade, with no aggregation rule**  
*Domain model / requirements*  
Every 'total' in the domain (GradingRecord = per-criterion scores + total) is per-ANSWER (one problem). There is no assessment-level or (student, assessment) grade entity, no storage, and no aggregation rule — yet Plan §5 shows a 'current score' column and §7 emails a 'total', and staff/students think in '73/100 for the exam'. Weighting (equal vs by max_points), rounding, and treatment of ungraded/blank/missing/absent problems (0 vs excluded vs partial/withheld) are all undefined, and recomputation on a single-problem regrade has no trigger.  
*Fix:* Define an assessment-level (and problem-level) grade: derived-view vs materialized (student, assessment) row; explicit sum/weighting rule (equal or by max_points); rounding; treatment of ungraded/blank/missing/absent as 0-vs-excluded-vs-withheld; and the recompute/invalidation trigger when any constituent official grade changes. Clarify what 'current score' means before totals exist.

**B-C3 — Publish workflow undefined: no state machine, granularity, lock, unpublish, or re-publish-after-regrade rule**  
*Grade lifecycle / workflow*  
Phase 6 is one line ('Finalize/publish; email breakdowns; embed regrade tokens') and the docs use 'official', 'published', 'finalized', 'finalize/publish' interchangeably with no defined distinction (Spec §4 even calls official_record_id 'the published record'). Unanswered: publish granularity (answer/problem/assessment/student — one email or five, can problem 3 publish before problem 5 is graded); whether publish freezes official_record_id; whether post-publish overrides send corrections; whether re-publish sends deltas; unpublish/undo after a spotted masking or grade error; and what blocks publishing a NULL/flagged official grade. Publishing sends irreversible individually-addressed student emails.  
*Fix:* Define an explicit answer/assessment lifecycle with named states (e.g. ungraded → ai-graded → human-reviewed → official-set → published(emailed) → regrade-open → superseded) and transitions; state whether 'official' and 'published' are one flag or two and what 'finalized' locks; capture a published snapshot (per-answer score/comment + published_at); recommend publish-per-student-when-all-answers-official as one consolidated email; define correction/re-send-on-change and unpublish semantics; record publish in the audit log with exact scores sent.

**B-C4 — Masking has no per-answer QA/verification gate before images reach the model**  
*Privacy & security*  
Masking is one fixed normalized rect per assessment/page-layout (Spec §4/§7) validated only by a single per-assessment TA preview. Batch fan-out (§6) masks every answer as (original + fixed region) with no per-answer confirmation. Real scans drift: name written outside the box, rotated/upside-down pages, recurring header name on page 2+, ID on a cover sheet/footer — the fixed rect misses and unmasked PII goes to a third-party LLM. This is the system's entire privacy invariant ('identity never reaches the model', §1), enforced by a rectangle and one preview, with no detection and no record when it fails.  
*Fix:* Mandate a verification gate: OCR/vision-detect roster name/student_id (fuzzy) inside AND outside the mask on the masked derivative and block/flag matches; verify (not just normalize) orientation and flag aspect/orientation outliers; require masking of ALL pages of a multi-page answer; make masked-copy review per-answer-batch (sampling + hard gate), not one per-assessment; on trip → quarantine, never auto-send.

**B-C5 — No ground-truth / spot-check / acceptance gate before trusting AI grades**  
*Grading correctness & fairness*  
Phase 4 sets official grades and Phase 5 auto-accepts on agreement, but nothing validates the AI against THIS course's handwriting/rubric first. No held-out human-graded sample, no target AI-vs-human agreement threshold gating auto-publish, no random spot-check of auto-accepted grades. Two models sharing a systematic misread will 'agree' and auto-accept — agreement is not accuracy — so confident wrong grades ship to students with zero human eyes; Phase 8 only measures override rate after the fact.  
*Fix:* Define a calibration/acceptance workflow: a per-problem human-graded ground-truth sample, a required AI-vs-human agreement threshold before auto-accept is permitted, and mandatory random spot-check sampling of auto-accepted grades (X% per problem) surfaced in the review UI.

**B-C6 — Answer identity on re-ingest/re-upload is undefined; grading history can be orphaned or attached to a stale image**  
*Domain model / ingestion*  
Re-uploading a corrected/late PDF is a routine correction path, but Answer identity is undefined. Is an Answer keyed by (student, problem) so a new page slots into the same row (preserving records graded against the OLD image), or does re-ingest create a NEW answer (orphaning history)? Records reference method/prompt/rubric versions but NOT the image version graded, so swapping image_ref silently makes every existing GradingRecord and the official grade claims about an image that no longer exists — breaking the reproducibility premise. Grading is idempotent (UniqueOpts(run_id, answer_id, model)) but ingestion has no idempotency key.  
*Fix:* Fix Answer identity as the natural key (student, problem), one Answer per pair per assessment; version the image (image_ref generation/hash) and record which image_ref each grading_record graded; on re-upload replace image as a new version while preserving identity and flag existing records as 'graded against superseded image' (or block replacement when graded/published) rather than silently re-pointing; define an ingestion idempotency key and deliberate GC/retention of superseded blobs; specify what happens to a published grade when its source image is replaced.

**B-C7 — The multi-model 'agreement rule' comparison semantics are never defined (incl. refusal/error quorum)**  
*Grading correctness & fairness*  
Plan §4/Spec §6.4 name an 'agreement rule (e.g. agree per criterion, or within a tolerance on the total)' but never define comparison granularity (per-criterion vs total — offsetting per-criterion errors can agree on total), aggregation (unanimous vs majority), tolerance value/unit, or the degraded-quorum case Plan §4 explicitly raises ('2 models agree, 1 refuses/errors'). Auto-accept vs flag-for-human — the decision NO human reviews — hinges entirely on this vague 'e.g.' string.  
*Fix:* Fully specify agreement config: per-criterion comparison required, aggregation (unanimous/majority), tolerance value + unit, min_responders, and a degraded-quorum policy (a model that refuses/errors counts as non-agreement → flag for human; never auto-accept on a subset unless min_responders met).

**B-C8 — Partial-credit granularity and total authority are undefined; scores may not add up**  
*Grading correctness & fairness / domain model*  
Two entangled atomic-unit gaps. (1) Granularity: points type (int/fractional) and legal increment (0.5? 0.25? arbitrary float?) are unspecified; Spec §5 clamps to [0, criterion.max] but not to a step, so models emit 3.0 vs 2.9 and 'agree'/'disagree' with no rule, and the UI control type (slider vs discrete) is undetermined. (2) Total authority: the model returns per-criterion scores AND a `total`; if they diverge (common LLM arithmetic error, each criterion individually in-range so no clamp catches it) which wins is unstated, and whether Σ(criterion.max)==problem.max_points is an enforced invariant is unstated — so published grades can silently exceed/fall short of the max.  
*Fix:* Add per-criterion scoring_mode (continuous | discrete with allowed_values/step) or a global increment (e.g. 0.5); snap/validate model output to legal values in Go before writing, recording any snap, and enforce the same increment in the human UI. Specify that the app authoritatively computes total = Σ(clamped per-criterion scores) and audits/ignores any model-supplied total; enforce Σ(criterion.max)==problem.max_points at rubric-save (or define the extra-credit/mismatch rule).

**B-C9 — No bulk 'set official grade' — finalizing 200 answers is a 200-click task**  
*UX & operator throughput*  
After a single-model run, every answer has AI records but official_record_id is still NULL. The docs describe setting the official grade only as a per-answer action in the Answer view; auto-set appears exclusively in Phase 5 agreement. So in Phase 4 — the first AI-grading milestone — accepting AI grades requires opening 200 answer views one by one, providing almost no throughput win over manual grading and blocking Phase 6 publish.  
*Fix:* Define a bulk 'accept AI grades as official' action scoped to a run or (problem, run) with predicate filters (unflagged, confidence ≥ X, single record only); specify which record it points to when multiple exist, how it treats answers already human-finalized (skip vs push-to-history), and that it is an audited action.

**B-C10 — Stored transcription / raw_output can itself contain unmasked identity, permanently and outbound**  
*Privacy & security*  
grading_records is immutable/append-only and stores raw_output plus a verbatim transcription the model is explicitly asked to produce (§4/§5). If masking missed a name, or the student wrote 'By Jane Chen, ID 12345' in the answer body, the model transcribes it verbatim into a forever-immutable JSONB column, and per §5/§8 the comment can flow into the emailed 'AI comment'. So even correct image-masking re-introduces PII into the DB and outbound email, and immutability means you can't delete it — colliding with any right-to-be-forgotten.  
*Fix:* Add a redaction/scrub pass on transcription+raw_output before persistence (strip roster-name/ID tokens); define an exception path to redact an immutable record when PII is found (append-only + tombstone rather than literal immutability); mandate transcription is never included verbatim in outbound student email; clarify whether full raw_output is retained or only a validated subset.

**B-C11 — Grades have no export path — the terminal step (marks to the registrar) is unsupported**  
*Requirements completeness*  
LMS integration is a non-goal (Plan line 65) and the only defined output channel is per-answer student emails. There is no CSV export, download, roster-wide report, or API. Staff who need 'a spreadsheet of every student's total for Exam 2' to submit to the gradebook have no way to produce it and will re-key hundreds of scores by hand — defeating the tool. 'No LMS integration' (no live sync) silently deleted ALL bulk export.  
*Fix:* Add a grades-export requirement: per-assessment CSV of student_id, name, per-problem totals, assessment total, official-grade source, published_at. Clarify the non-goal means 'no live API sync', not 'no export file'. Small, well-scoped, belongs in Phase 6 or 8 (and depends on the assessment-total definition).


### High (20)

**B-H1 — Status vocabulary and answer→problem→assessment aggregation are asserted but never enumerated**  
*Domain model / requirements*  
answers.status and assessments.status are real DB columns and drive every list badge, progress counter, and publish/regrade check, but no enum is defined and the answer→problem aggregation ('derived aggregate', Plan §3) is undefined. The Plan's examples mix mutually-exclusive lifecycle states (ungraded/AI-graded) with orthogonal dimensions (has-flags, has-open-regrade, published) that can be simultaneously true — so they cannot be a single enum. Two developers (or one across sessions) will invent incompatible sets and rollups, making list views, counters and 'ready to publish' checks inconsistent.  
*Fix:* Enumerate the canonical Answer status lifecycle as a proper state set; split orthogonal booleans (flagged, has_open_regrade, published) out of the lifecycle status; define the exact answer→problem and problem→assessment aggregation (priority/worst-of or a multi-count summary, not a single enum); enumerate assessment.status and who/what transitions it. Reconcile with the publish/official state machine.

**B-H2 — Reproducibility is claimed but undermined by unpinned model nondeterminism and mid-term model retirement**  
*Grading correctness & fairness*  
Spec pins a model id (e.g. gemini-2.5-flash) but says nothing about temperature/top-p/seed. At temperature>0 a re-run on byte-identical input returns different scores, so 're-running the same method on the same answer' produces a different record — falsifying the reproducibility/explainability guarantee used to defend grades on appeal. Separately, a pinned model retired mid-term (Plan §12 flags, neither resolves) silently changes the 'same method' within one assessment and leaves regrades comparing against a dead model.  
*Fix:* Add temperature/top-p/seed to GradingMethod config, default temperature 0 for grading, and record them + the resolved concrete model version string on every record. Define a model-retirement policy: freeze the resolved model per assessment; on retirement require an explicit human-acknowledged method migration, not silent substitution.

**B-H3 — Human override racing an in-flight run can silently clobber the human official grade (guardrail unenforced)**  
*Grading correctness & fairness / concurrency*  
Plan §10 forbids a re-grade silently overwriting a human-finalized/official grade, but official_record_id is a mutable pointer and Spec §6.4 Reconcile 'auto-set[s] official grade on agreement' with no check for 'answer was human-finalized after the run started'. The moment problem-scope runs and manual grading coexist (the normal workflow), a model can clobber a TA's authoritative decision, or a bulk run produces AI records for human-finalized answers with no 'human-finalized, AI ran anyway' flag. Immutable records also lack any 'retracted' marker, so typo corrections accumulate as noise.  
*Fix:* Define official-grade precedence: a human-sourced official grade is locked; Reconcile and run-planner must skip (or flag-not-overwrite) any already-human-finalized answer, checked as 'official record source == human' at pointer-set time inside the same transaction, and logged. Decide run-planner behavior for human-finalized answers (recommend: still grade for comparison but never move the pointer). Add a superseded/retracted marker (or treat 'official pointer moved away' as the retraction signal).

**B-H4 — Batch-path partial completion, mid-run failure, resume, and 're-run failed leaves only' are under-specified**  
*Operations & workflow*  
At 1–3k calls/run partial failure is normal (Spec §2 admits ~99.8% conformance, plus 429s/timeouts/refusals-past-cap). Spec §6 doesn't define: what PollBatch does with per-item batch errors (run → completed with holes? stuck running? failed discarding good results?); the provider batch id isn't stated as persisted, so a VM reboot mid-batch orphans/double-charges it; River backoff (attempts^4) has no stated max-attempts or terminal outcome; and there is NO operator action to enqueue only the missing/failed leaves — the only launch path re-grades all 200. Batch's multi-hour latency makes a wedged run invisible for a long time.  
*Fix:* Define run terminal states precisely (run completes only when every (answer, model) leaf has a record or a recorded terminal error); add a per-item batch-error path (re-queue to sync or record a failed leaf with the provider error); persist the provider batch id on the run/leaf and reconcile in-flight batches against provider status on startup; set max retries per leaf, a run-level failure threshold, and a 'retry incomplete/failed leaves of run N' operator action plus a run-detail view of per-leaf outcomes.

**B-H5 — No LLM cost budget caps or pre-flight spend guardrails**  
*Operations & cost*  
Cost is tracked per-record AFTER the fact, but there is no pre-flight estimate, no per-run/per-term cap, and no kill-switch. A premium arbiter (Opus/GPT-5.5) at reasoning=high over 200×8=1,600 calls, a multi-model method running 3 models, or an accidental whole-assessment re-run silently burns real money with no ceiling or confirmation — exactly the surprise-bill gap on a self-funded project, made one-click by the premium presets.  
*Fix:* Add a pre-launch cost estimate (calls × model × est tokens) at run confirmation; configurable per-run and per-term budget caps that block/queue-for-approval over-budget runs; a running per-term spend total in the UI; and a hard global monthly cap that pauses provider queues when hit.

**B-H6 — No CSRF/state (nor nonce/PKCE) protection on the OAuth login/callback**  
*Security*  
Spec §2/§10 verify aud+hd+allowlist and use scs sessions with SameSite=Lax, but never mention the OAuth `state` parameter, PKCE, or an ID-token `nonce`. Without a verified state bound to the pre-login session, the single auth entry point to all student grades/PII is open to login CSRF / session fixation; SameSite=Lax does not cover the top-level GET to the callback. The explicit auth checklist omitting these invites a build that skips them.  
*Fix:* Require a cryptographically random `state` stored server-side / in a pre-session cookie and verified on callback; ID-token `nonce`; PKCE; and session-ID rotation on successful login (fixation defense) — alongside the existing aud/hd/allowlist checks.

**B-H7 — No data-retention, deletion, or right-to-be-forgotten policy for education records**  
*Privacy & compliance*  
Neither doc defines retention windows or a purge path for submissions, JPGs, grading_records (with transcription), regrade emails, or audit logs, and Guardrails (§10) push toward immutable/append-only 'kept forever' — the opposite of deletability. A student invoking deletion, or an end-of-term limit, has no supported operation, yet PII lives in submissions, answers, grading_records, regrade_requests, audit_log, River payloads, the session store, AND the email provider. Handwritten exams tied to name+ID+email are education records that almost always carry a retention/deletion obligation.  
*Fix:* Add a retention & erasure section: per-entity retention windows; a documented 'purge student' operation cascading across blobs, DB rows, River payloads, audit log (compliant tombstone), and the email provider; scope immutability to grade integrity (not raw PII) so legally required erasure is possible.

**B-H8 — Blank/illegible answers and absent (no-submission) students have no domain representation**  
*Domain model*  
Every assessment has no-shows and blank/illegible pages. If Answers are created only on upload, the 7 of 150 who didn't submit can't appear in a problem's student list, can't be scored 0/absent, and silently drop from any assessment total. Spec §5 has an 'illegible/uncertain' prompt path, but the DOMAIN has no status distinguishing 'blank = 0' vs 'illegible = needs human' vs 'not attempted', no rule for whether blank auto-scores 0 or flags, and no audit of who set an absence/zero.  
*Fix:* Decide whether Answers are pre-materialized for every (rostered student, problem) at ingest (missing = a distinguishable no_submission state) or created on upload with an explicit UI surface for missing students; add Answer states no_submission / blank / illegible; define whether blank auto-scores 0 or flags for human; record who set an absence/zero.

**B-H9 — Roster CSV↔filename mismatches, duplicates, and the CSV contract are unspecified**  
*Domain model / ingestion*  
Identity maps via filename <studentID>.pdf, but there's no defined resolution for: a file whose studentID isn't in the roster (reject/quarantine/orphan?); two files for one studentID (A.pdf vs A(1).pdf — which wins?); roster re-import upsert key (student_id vs email) and email changes after tokens bound to the OLD roster_email were issued; duplicate roster rows. The CSV schema itself (columns, header, encoding, required/unique fields) is undefined. Mis-association means a student sees someone else's grade or a regrade token points at the wrong person.  
*Fix:* Define the roster CSV contract (exact columns, required/unique, encoding); choose student_id as the unique identity/upsert key with defined re-import semantics; specify filename→roster misses as a quarantine list surfaced in the mapping UI (never silent orphan), multiple-files-per-student resolution, and email-change handling (existing tokens bound to old email must still verify or be deliberately invalidated).

**B-H10 — Inbound regrade token: replay, non-roster sender, and roster-email change are unhandled**  
*Security / fairness*  
The reply token embeds {run_id, answer_id, student_id, roster_email, issued_at, nonce}; inbound verifies HMAC+expiry+sender==roster_email+SPF/DKIM (§9). Gaps: replay — the same valid token/email resubmits and can drive up to 3 auto-regrades; a nonce is stored but no single-use/consumed check is described. Forwarding — a legit reply from a personal address is silently blocked with no recovery. Stale email — roster_email is baked in at issue, so correcting it makes every already-sent token fail sender-match forever with no re-issue path. Replay changes published grades; the silent drops kill legitimate appeals.  
*Fix:* Specify token single-use / nonce consumption (or a strict per-token rate cap independent of the 3-strike answer cap); define behavior + student-facing messaging when sender != roster_email (human-review queue with identity re-verification, not silent drop); define a token re-issue path on roster_email change; consider binding fewer PII fields (student_id suffices; roster_email in the token is PII in every reply address and inbound log).
*Status (2026-07-03 N2 wave):* the "stale-email → token dies forever" break is now mostly closed by the **token re-bind** (DECISIONS addendum, C3): a superseded-batch token re-binds to the same student's live-batch item across an unpublish→re-publish, so a re-publish re-arms outstanding links instead of killing them. Remaining open: only the `postmark` provider parses inbound replies — running `smtp` with `ADAMARKER_EMAIL_REPLY_DOMAIN` set advertises a Reply-To no webhook can receive (loud boot warning + OPERATIONS.md note, I4); forwarding-from-a-personal-address is still a silent reject; roster-email-change with no live re-publish is still a dead link until re-publish.

**B-H11 — Secrets management and rotation on the single VM are undefined**  
*Security / operations*  
The design relies on long-lived secrets (HMAC server_secret for regrade tokens, Google OAuth client secret, provider keys, Postmark keys, scs session key, Postgres creds) but Spec §3 says only 'env + secrets loading' — no storage, scoping, or rotation. If server_secret leaks (logs/backup/config) an attacker forges regrade tokens for any (student, answer) to drive grade changes. Worse, rotating server_secret invalidates ALL outstanding regrade tokens (an unaddressed operational break), and there is no key-per-purpose separation.  
*Fix:* Specify secret storage (0600 perms / systemd credentials / OS keyring, not a world-readable env dump); a key-ID scheme so server_secret rotates with a grace window (verify current+previous); separation of session key vs token key; least-privilege per-provider key scoping (Vertex no-train project); and a documented rotation procedure per secret noting which rotations invalidate outstanding artifacts (tokens, sessions).

**B-H12 — PII in logs, SSE, and River job payloads is unguarded**  
*Privacy / observability*  
There is no logging/observability policy (Spec §3 has no such seam). Handwritten answers, transcriptions, name/ID/email, and full raw_output flow through render/ingest/mask/grade/email/SSE. Default behavior of Go services + River (which persists job args/errors in Postgres) + provider SDK error logs spills answer content, transcriptions, and PII into app logs, River job tables, and traces; the signed reply token (containing roster_email) also lands in web access logs. These become an uncontrolled secondary PII copy outside any retention/deletion story.  
*Fix:* Add a logging policy: structured logs with an explicit field allow-list; never log answer content/transcription/raw_output/name; scrub tokens and emails from access logs; require River job args to reference PII by id only (no content in payloads) and redact error messages; set log retention alongside data retention.

**B-H13 — Multi-page answers contradict the one-page Answer model with no defined representation**  
*Domain model*  
The Answer holds singular source_page_ref/image_ref (usually one page), yet Plan §6 and Spec §7 explicitly call out 'answers spanning two pages' as a normal case the manual UI must handle. When one (student, problem) occupies pages 4 AND 5, the single-ref columns can't hold the second page, the masked derivative and vision call assume one image, and the grader silently drops half the student's work — a direct correctness failure on a case the docs promise to handle. (Also blocks proper multi-page masking, per the masking QA gap.)  
*Fix:* Model an Answer as owning an ordered set of page images (answer_pages: answer_id, page_index, source_page_ref, image_ref, masked_image_ref) rather than single refs; define how the grading prompt presents multiple images and how masking applies per page; this also handles multiple problems on one page by letting a page feed more than one answer.

**B-H14 — Whole-cohort systematic misread has no detection mechanism**  
*Grading correctness & fairness*  
If the reference solution for a problem is wrong, or the model systematically misreads a course-wide notation, ALL 150 students lose the same points on one criterion. Every individual grade looks confident and internally consistent, per-answer confidence flags won't fire, and multi-model agreement may even confirm it. All the design's safety nets are per-answer, so this highest-blast-radius fairness failure is precisely the one they cannot catch — it surfaces only via a flood of post-publish regrade emails.  
*Fix:* Add per-problem/per-criterion score-distribution summaries in the review UI before publish (mean, spread, %-at-zero, %-at-max) plus an anomaly flag when a criterion's distribution is degenerate, so a human sanity-checks the aggregate before grades go out.

**B-H15 — Appeals fairness: whether a regrade may LOWER a grade is never stated**  
*Grading correctness & fairness / policy*  
On a regrade, the auto-re-run of a method (or a human review) may score lower than the current official grade. Neither doc states a no-detriment (ratchet) policy or its opposite — a first-order fairness/legal decision (many institutions forbid appeals from lowering grades; others allow it). Because the system auto-re-runs methods on regrade, absent an explicit rule it will happily publish a lower score from the student's own appeal, and the resolution-email/score-change loop (contents, disclosure, whether a live vs terminal token is attached, strike-count interaction) is likewise undefined.  
*Fix:* Add an explicit regrade-direction policy (e.g. 'auto-regrade may raise or flag, but never lowers the official grade without human confirmation') and encode it where official_record_id is updated on the regrade path. Specify resolution-email contents (new total + breakdown + what changed), decrease disclosure, whether the resolution email carries a live reply token or a terminal 'no further regrades' notice (esp. post-escalation), and how strike-count interacts with the token.

**B-H16 — No concurrency control for two TAs grading the same problem**  
*UX / concurrency*  
The workflow encourages grading a whole problem across students, so the natural way to split 200 students is two TAs on the same problem. Records are append-only (safe), but official_record_id is a single pointer: TA-A sets it to their manual record while TA-B sets it to the AI record seconds later — silent last-write-wins, neither warned. No locking, presence indicator, optimistic-concurrency check, or claim/assign partitioning exists; at 2–4 TAs on 200 students this happens routinely.  
*Fix:* Add optimistic concurrency on official_record_id updates (reject stale writes with a 'record changed, reload' UI), plus a lightweight presence/soft-claim indicator on the student row, or an explicit assignment mechanism to partition a problem's students across TAs.

**B-H17 — Post-ingest reconciliation + fast masked-crop review are missing; flags have no triage queue**  
*UX / operator throughput / privacy*  
Between ingest and grading the operator must verify mapping and that the mask covers the name on EVERY scan, and after a run must triage flags. But the docs give tools without a throughput workflow: no view of 'unmapped / duplicate-student / zero-page / roster-student-with-no-submission', the mask preview is one per-assessment sample while scan drift misses individual crooked scans, and flags are only row-highlights within per-problem lists (no cross-problem list, no filter by reason, no sort by score-delta, no bulk-resolve, no modeled flag lifecycle). Reviewing 200 masked crops one by one is the implied path; a missed mask leaks PII and a mis-map sends A's grade to B — both silent.  
*Fix:* Specify an ingestion-reconciliation view (roster vs submissions: missing/extra/duplicate/unmapped/page-count-mismatch) as a gate before grading; a fast keyboard-navigable masked-crop review (accept/flag per answer) instead of one per-assessment preview; and a filterable/sortable cross-problem flag view (by reason, by score-delta) that deep-links into the answer view, plus a modeled flag lifecycle (open → resolved/dismissed, by whom).

**B-H18 — A mid-run method/rubric/prompt edit can corrupt an in-flight run**  
*Workflow / reproducibility*  
Versioning protects history and runs pin rubric_version, but a run spans minutes-to-hours (batch latency). If a TA edits the GradingMethod/prompt while a run using it is running/paused (creating a new version), it is unspecified whether still-queued leaves read the snapshot frozen at PlanRun or re-resolve to the new version. Re-resolving mixes two method versions across one run with no visibility, silently violating 'consistent standard within a problem' and making the run non-reproducible during exactly the active-experimentation the product optimizes for.  
*Fix:* State explicitly that a run snapshots its method+prompt+rubric versions at launch/PlanRun and all leaves use that frozen snapshot; optionally warn/soft-block editing a method that has a non-terminal run; surface which method version each run used.

**B-H19 — No availability target and undefined restart/recovery for in-flight runs**  
*Operations & reliability*  
The single VM is an explicit single point of failure, yet no availability/uptime expectation is stated, so no one can judge whether the design is adequate (e.g. 'grading may be down N minutes during exam week'). On reboot mid-run, River re-picks pending jobs, but whether a job re-attaches to an already-submitted provider batch (whose id must be persisted) or orphans/double-charges it is undefined; 'progress is restart-safe' is asserted but provider-side batch-handle persistence isn't.  
*Fix:* State an explicit (even best-effort) availability target and maintenance-window expectation; persist the provider batch id on the run/leaf so PollBatch reattaches after restart; on startup reconcile any run in 'running' against provider batch status before enqueuing new work; document expected reboot-mid-run behavior.

**B-H20 — Rubric re-versioning leaves prior official grades, displayed breakdowns, and cross-run reports undefined**  
*Domain model*  
Editing a rubric v1→v2 (drop criterion B, add D) preserves old records but creates a live consistency problem: an official grade still pointing at a v1 record scored on {A,B,C} — is it now 'stale' vs the current rubric? The Answer view must render a v1 record whose criterion B no longer exists and whose D was never scored. Does editing the rubric auto-advance the default method's pinned rubric_version? Phase 8 reports may compare scores across different criterion sets with the same max — how is agreement/override-rate computed across versions? None is specified; TAs refine rubrics mid-grading as a normal occurrence.  
*Fix:* Specify: whether an official grade under an older rubric version is marked 'outdated' when the problem's current rubric advances; that the Answer view renders each record against the version-as-graded (not the current rubric); whether editing a rubric bumps the default method's pinned rubric_version and whether that re-flags existing grades; and that Phase 8 comparisons are restricted to the same rubric_version.


### Medium (21)

**B-M1 — Publish-time completeness gate is undefined — partial/failed runs could email students** — A run can finish 'completed' while some answers failed (past cap, refused, batch item errored, unmapped/blank). Nothing states that Publish verifies every in-scope (rostered student × problem) has an official grade in a publishable state before any individually-addressed email goes out. Without a 100%-coverage gate, students get wrong or missing totals discovered only via replies — where upstream gaps (refusals, mapping blanks, overrides) converge into a student-visible failure. (Overlaps the publish-state-machine gap; this is specifically its coverage precondition.)
**B-M2 — Manual-vs-run coexistence and record retraction are under-modeled** — grading_records.run_id is nullable for manual grades. A TA manually grades (record M, official), then an assessment-wide run includes that answer (record R). Plan §10 says the run must not silently overwrite the human official grade, but the Spec §6 planner fans out over the scope's (answer, model) set with no mention of excluding human-finalized answers — so it's unspecified whether the run skips, produces R but leaves official on M, or flags. Also, immutable records mean a human's typo correction is a new record while the wrong one lingers forever with no 'retracted' marker. (Same mechanism as the override-race gap; this is its manual-grade-first direction plus the retraction need.)
**B-M3 — Submission↔Answer relationship and 'replace submission after grading' lifecycle are ambiguous** — This is the concrete data-lifecycle face of the plan-vs-spec Answer-parentage contradiction (Plan §3 Submission 1—* Answer vs Spec §4 answers with no submission_id). It determines: whether an Answer re-points to a new submission's pages on re-upload; whether the OLD source PDF/pages are retained for the prized audit trail or deleted; and whether a student submitting problems across TWO PDFs is even representable (the 1—* shape breaks). Left unresolved, ingest/re-ingest have undefined data lifecycle.
**B-M4 — Masked-image-only-to-provider invariant has no enforcement point or null-ref failure handling** — Spec §7 'Only the masked JPG is ever sent to a provider' is prose, not a constraint: VisionProvider (§3) takes image bytes and nothing stops a caller (grade worker, re-ask path, batch assembly, arbiter preset) from passing image_ref. There is also no defined behavior when masked_image_ref is null (masking failed / not yet run) — a natural 'use image_ref' fallback would send the UNMASKED original. The privacy guarantee reduces to 'the developer always picks the right field.'
**B-M5 — No disk capacity planning, free-space monitoring, or derivative GC policy** — 200 students × 6 assessments × ~8 pages × 3 artifacts (source page + rendered JPG + masked JPG) ≈ 29k artifacts/term at 200–300 DPI, tens of GB/term, growing every term (nothing deleted). No disk-sizing guidance, no free-space monitoring, no GC/retention for superseded masked derivatives. On one VM a full disk halts ingestion, masking AND Postgres (same box), taking the system down mid-exam. The masked JPG is a pure derived artifact, so store-vs-recompute is a live choice driving growth.
**B-M6 — Monitoring/alerting beyond healthz is absent; stuck runs / dead providers go undetected** — The only observability mentioned is per-run SSE in the UI, which requires a human to be watching. A run stuck 'running' for 6h on silent rate-limiting, a looping PollBatch, crashed River workers, low disk, or a down DB triggers no alert. For a solo-maintained system, silent stalls are the most common real failure and are discovered only when a student complains their grade never arrived.
**B-M7 — Provider outage / rate-limit exhaustion mid-run has no give-up or escalation path** — River retries with attempts^4 backoff, but the max attempt count and the terminal outcome on exhaustion are unstated: does a leaf become a failed record, get dropped, or block the run forever? And how many of a 1,500-item run can fail before the run itself is declared failed? Rate-limit/outage is the single most likely mid-run event at this volume; without a defined exhaustion policy and a re-run/reassign action, a partial outage leaves the run ambiguous and answers ungraded with no remediation. (Closely tied to the batch partial-completion gap.)
**B-M8 — Malformed/refused output past the re-ask cap has no defined grade outcome** — When a model returns invalid JSON or takes the 'illegible/uncertain' refusal path twice past the cap, what record is written — none, a zero-score, or a 'needs-human' status? A grade of 0 and 'unable to grade' are opposite fairness outcomes (one wrongly penalizes, one correctly routes to a human). If no record is written the answer may silently stay ungraded and be published as missing/0; and its effect on multi-model agreement quorum is undefined (ties to the agreement gap).
**B-M9 — Batch item isolation is not stated to carry the per-(answer,model) leakage invariant** — The 'fresh session per (answer, model) so no answer leaks into another's grading' invariant (Plan §4/Spec §5) is asserted for sync but not explicitly carried into the batch path, which 'group[s] items into provider Batch submissions' (§6). Standard batch APIs isolate items so it likely holds, but the spec never states each batch item is a self-contained single-answer request, and a naive token-saving implementation could concatenate answers into one prompt — a near-invisible cross-answer fairness bug.
**B-M10 — Grade scale, points type, and Σ(criteria)==max invariant beyond partial-credit are unspecified** — Complementary to the partial-credit granularity gap, at the schema/type level: are points integer or numeric (drives a hard-to-change column type once records exist)? Is there any letter/scale mapping in v1? Is sum(criteria.points)==problem.max_points an enforced rubric-save invariant, and what is the rounding rule when problem totals sum to an assessment total? Inconsistent types/rounding produce grades that don't add up and off-by-a-fraction totals across problems.
**B-M11 — Problem number/order relationship and post-grading renumbering vs positional ingest mapping are unconstrained** — problems has both `number` and `order` with no stated relationship or uniqueness of `number` within an assessment. The ingest page-layout mapping is positional (Spec §7); if problem order/number can drift after it's configured (a TA inserts/renumbers after answers/grades exist), later uploads or re-ingests map pages to the wrong problem, and already-sent emails referencing 'Problem 3' diverge. Two number fields with no invariant invite display-vs-identity inconsistency.
**B-M12 — Student-facing email content contract is undefined — unreviewed AI text may reach students** — Students interact 'entirely by email' and the breakdown includes per-criterion scores and AI comments. Unanswered: are AI comments shown verbatim (a hallucinated/harsh justification unreviewed)? Is AI involvement or the model name disclosed? Are reference solutions ever exposed? Is there a human-approval gate before an AI comment is emailed? The plan redacts student identity FROM the model but never redacts model artifacts TO the student — emailing raw model text is an academic-integrity/PR risk the spec literally implies.
**B-M13 — Open regrades vs assessment archival have no defined interaction (silent orphan/deadlock)** — Archiving soft-deletes an assessment ('disappears from normal views, retains data'), but if students have open RegradeRequests — one escalated to mandatory human review — nothing defines the interaction. A regrade token stays valid and the inbound webhook still fires: a student emails in, gets an auto-confirmation, but the assessment is invisible so no TA ever sees the queued item (silent orphan), or the inbound handler errors creating a RegradeRequest against an archived assessment. The docs specify archival and regrades independently but never their collision.
**B-M14 — Timezone/clock convention for token expiry, deadlines, and email timestamps is unspecified** — Regrade tokens have issued_at + expiry and regrades escalate after a threshold, but no timezone is fixed: is expiry UTC or campus-local? If a regrade window is deadline-bound, which clock? A student in another timezone replying at a boundary is accepted or rejected on an unstated convention, and student-facing email timestamps/deadlines are undefined — exactly where deadline/expiry disputes generate appeals.
**B-M15 — No data-scoping between TAs — every TA sees every student's identified submissions** — Roles are Admin/Lecturer/TA with uniform TA data access, and the Answer view always shows the ORIGINAL unmasked image to humans. So any allowlisted TA can browse every student's name-attached handwritten answer, grade, and regrade correspondence across all assessments. There's no per-problem/per-student TA assignment and no acknowledgment of whether that breadth is acceptable — a real need-to-know / conflict-of-interest control (TAs are often adjacent students) that is simply absent, and expensive to retrofit as row-level scoping later.
**B-M16 — Email provider is a second uncontrolled PII store outside the retention/deletion story** — Outbound grade emails carry name + per-criterion breakdown + comments + a token embedding roster_email; inbound replies (possibly quoting the answer) are stored as regrade_requests, and Postmark retains message content/logs by default. The privacy analysis focuses on the LLM provider (§13), but the email provider receives arguably more identifying data and sits outside any retention/deletion story and processor inventory — for an EU/other-jurisdiction institution it needs the same scrutiny as the LLM.
**B-M17 — Publish/finalize gate does not require masking QA to have passed** — Publish/email is gated only on grade state (grading_runs, official_record_id), with no precondition that masking succeeded and verification passed for every published answer. Combined with the missing per-answer masking gate and the null-ref fallback risk, an answer could be graded from an unmasked/mis-masked image and then published with no checkpoint at the irreversible, student-facing last line of defense.
**B-M18 — No keyboard-fast grading affordances for the high-volume manual path** — Phase 3 (zero-AI manual grading) may have a TA hand-grade 200 answers, yet the Answer view is described entirely in mouse/drill-down terms with a list round-trip between every student. No next/prev-answer, next-ungraded, next-flagged, keyboard per-criterion entry, or save-and-advance. For the explicit zero-AI fallback and for flag triage this is punishing at 200 answers and is the difference between the tool being used and TAs reverting to paper/spreadsheets.
**B-M19 — Method-vs-method comparison workflow is undefined (only per-answer history exists)** — Trying multiple methods and comparing is a core selling point, but the only concrete comparison affordance is the per-answer record-history list. The operator's actual decision — 'method A vs B across the whole problem: where do they disagree, score-distribution delta, which matches my human grades best' — has no surface until Phase 8's vague 'cross-method comparisons'. Comparing by opening 200 answer histories is not a workflow, risking the marquee feature being unusable.
**B-M20 — Calibration on assignments-before-exams has no data-model hook or defined metric** — Both docs list 'do assignments calibrate methods before exams?' as open, but the model has no way to express it: no notion of a method 'validated on assignment N', no quality metric attached to a GradingMethod, and no defined metric (agreement with human? override rate? per-criterion error?) by which one method is judged before a high-stakes exam. Objective 8 is 'measure & improve', but without a metric and a method↔performance linkage TAs pick exam methods on vibes.
**B-M21 — Batch-vs-sync threshold value is never bound** — The decided mode is 'Batch by default for multi-answer runs; sync for single-answer/interactive', and §6 picks Batch 'when the set exceeds a threshold' — but the threshold number is never given. A TA re-grading one problem across a 5-student tutorial group: batch (multi-hour latency) or sync? Batch trades ~50% cost for up-to-24h latency, so the boundary being wrong makes small interactive runs feel broken or forces large runs to expensive/rate-limited sync. (This is one of the explicit dangling decisions.)

### Low (3)

**B-L1 — Reference-solution version is not pinned to records — reproducibility hole** — reference_solutions is versioned and a GradingMethod carries a COUNT of them, and records store method+prompt+rubric versions but NOT a reference-solution version. Reference solutions are part of the prompt and directly shape the grade, so a record is reproducible w.r.t. rubric/prompt/method but NOT w.r.t. which reference content was fed to the model. Editing a reference solution after grading makes re-runs silently use different reference material with no version trail — falsifying the load-bearing reproducibility guarantee.
**B-L2 — Empty / first-run states unspecified, including the allowlist admin-bootstrap lockout** — First-time experience is undefined across every screen, and one case is a real lockout: Spec §10 denies unlisted emails entirely with no seeded admin, so on a fresh deploy nobody can log in to add themselves to the allowlist (chicken-and-egg). Also undefined: empty assessment/problem/student lists and a brand-new GradingMethod screen. Empty states are cheap to specify and painful to discover in production; the bootstrap lockout is a genuine deploy blocker.
**B-L3 — Migration rollback story is absent; goose runs in-process at startup** — Migrations run automatically at startup on the single production VM; a migration that fails halfway or ships a bad schema change can wedge startup with the app down and no defined rollback (down migrations? pre-migration dump? blue/green?). Lower pain than data loss because it's deploy-time and detectable, but the procedure must exist before the first schema change ships.

---

## Deferred workflow hazards (audit 2026-07-10)

*The 2026-07-10 inconsistent-state hazard audit (origin of the workflow-guards plan,
[`superpowers/plans/2026-07-10-workflow-guards.md`](superpowers/plans/2026-07-10-workflow-guards.md))
confirmed 30 high-severity hazards; most are being surfaced as derived-state warnings at the
Launch/Publish chokepoints plus standing banners. The nine below were confirmed but deliberately
NOT given a warning in that round — each needs either a workflow decision or new mechanism, and a
banner would be the wrong tool. One paragraph each: current state → risk → why deferred →
suggested future fix. File:line citations were re-verified against the tree as of 2026-07-10;
functions are named so drift doesn't strand them.*

**W1 — One-page-per-cell scan model vs genuine multi-sheet answers** — Scan intake places exactly
one live page per (student, problem) cell: `placeAuto` parks a second identified page for an
occupied cell instead of ever overwriting (D64/D65, `internal/scan/identify.go:319-338`), and
`ResolveConflict` (`internal/scan/mutations.go:269-275`) offers only keep (discard the parked
page) or replace (supersede the incumbent — per-problem promotion supersedes the prior pages,
`internal/ingest/ingest.go:271-274`), never both. A student who legitimately writes one problem
across two sheets loses a sheet no matter which button the TA clicks, and is then graded on half
their work — a silent per-student fairness failure the parked-conflict card now at least states
honestly ("Keeping one page discards the other", Task F1). Deferred because the answer model can
already hold multiple ordered pages (`answer_pages`, D1) but the entire scan pipeline — the
cell-uniqueness partial index, conflict parking, promote-supersede — is built on one-page-per-cell;
a "keep both" resolution needs a new placement action with page ordering, mask seeding, and
grading-prompt handling for the appended page, i.e. a design round, not a warning. Future fix: an
"append as extra page" conflict resolution that attaches the parked page as an additional
`answer_pages` row behind the incumbent, reusing the existing multi-image grading path.

**W2 — Concurrent manual grading is last-write-wins at the fallback derivation** — Round-based
grading removed the user-written official pointer entirely ("no set_official path anymore",
`internal/httpapi/review.go:398-403`); a manual grade is an append-only record, and
`RecomputeOfficials` picks the fallback as the LATEST human record on the current rubric
(`ORDER BY gr.id DESC LIMIT 1`, `internal/store/queries/grading.sql:21-68`). Two TAs splitting the
fallback queue can grade the same answer minutes apart; the later insert silently becomes the
official fallback and the first TA's grade survives only in record history nobody re-opens — B-H16's
scenario, resurrected one level down (D6's optimistic concurrency guarded the old pointer write,
which no longer exists). Deferred because no chokepoint exists to warn at — the collision is
between two grading forms, not at Launch/Publish — and the honest fix is a concurrency mechanism,
not copy. Future fix: submit the answer's latest-record id with the manual-grade POST and 409 when
a newer human record appeared meanwhile ("someone graded this while you were typing — review theirs
first"), optionally plus a soft-claim/presence hint on the review list.

**W3 — Per-item resend sends to the publish-time email snapshot** — Publish captures each
student's recipient address into `publish_items.recipient_email` at publish time
(`internal/store/queries/publish.sql:9`, read back by `GetPublishItemForSend`
`publish.sql:58-67`; `internal/publish/snapshot.go:89` documents the capture), and `ResendItem`
(`internal/publish/publish.go:558-575`) just re-enqueues that row's send job. The flagship resend
use-case is "student says they never got it" (D46) — but when the cause is a wrong roster email,
the operator fixes the roster, clicks resend, and the mail goes to the same dead address; the loop
looks closed while the student still has nothing. Deferred because the snapshot address is
deliberate (B-H10 anchors the batch's token story to it) and switching resend to the live roster
address interacts with inbound sender-matching — rung 3 already checks the CURRENT roster email
(`internal/httpapi/regrade.go:122,207`), so re-reading the roster is likely correct but deserves
one deliberate decision, not a drive-by inside a warnings task. Future fix: on resend, compare the
item's `recipient_email` to the current roster email; when they differ, update the item's address
(audited) or at minimum surface a "roster email has changed since publish" confirm.

**W4 — Deactivating a user who is an assigned regrade TA orphans handed-off appeals** —
`handleUpdateUser` (`internal/httpapi/api.go:563-617`) guards only self-lockout;
`problem_ta_assignments` rows are untouched, `ListTAAssignments` never joins `users.active`
(`internal/store/queries/regrade.sql`), and the final-turn handoff notifier resolves the assignee
via `GetProblemTA` + `GetUserByID` with no active check (`notifyHandoffTAs`,
`internal/httpapi/regrade.go:392-470`). A deactivated TA's sessions are killed and login denied
(`api.go:371`), so handed-off regrade requests keep routing to someone who can never see them —
the queue shows "assigned", the D60 unassigned-problem warning stays silent, and a student's
final-turn appeal sits unowned until they escalate out-of-band. Deferred because the right
behavior is a workflow decision (block deactivation? auto-unassign? force reassignment?) spanning
the users and regrade seams, not a banner. Future fix: on deactivate, look up the user's problem
assignments and open handed-off requests and require explicit reassignment — or auto-delete the
assignment rows so the existing publish-preview `unassigned_ta_problems` warning starts firing.

**W5 — Records from cancelled runs stay eligible to become official grades** — Cancelling a run is
just `SetRunStatus('cancelled')` (`handleCancelRun`, `internal/httpapi/runs.go:412-431`); leaves
already written keep their `grading_records`, and `RecomputeOfficials`' source lateral selects the
latest model record via any version of the final method with no join to `grading_runs` at all
(`internal/store/queries/grading.sql:29-49`). The operator who cancels a mid-flight run precisely
because it was misconfigured (wrong prompt, wrong model) still gets its partial output silently
promoted to official wherever those records are newest. Deferred because excluding cancelled-run
records changes the officials-derivation semantics, which the workflow-guards plan's global
constraints explicitly freeze — and the rule is genuinely ambiguous (a run cancelled at 95% for
cost reasons may hold perfectly good grades). Future fix: make cancel ask "keep or retract this
run's records", with retraction a record-level flag the officials query excludes; or, cheaper, a
workflow warning counting current officials whose record belongs to a cancelled run.

**W6 — Republish/resend-all reverts adopted regrade overlays in the grade email** — A "regraded"
verdict adopts an overlay record on the regrade sub-item only
(`internal/httpapi/regrade.go:875-957`); the result email's new score reads that adopted record
(`regrade.go:1162-1197`) while `official_record_id` is never moved (published answers are locked,
`RecomputeOfficials ... WHERE published_at IS NULL`). But `Publish` rebuilds every snapshot from
`official_record_id` alone (`buildSnapshots` over `PublishSnapshotInputs`,
`internal/publish/publish.go:411-415`, `internal/store/queries/publish.sql:284-294`), so an
unpublish→re-publish or a resend-all re-emails overlay-adjusted students their PRE-regrade scores —
directly contradicting the result email they already received — and the changed-only diff
(`publish.go:415-429`) compares two official-derived snapshots, so overlay adoptions can never
register as a change either. Deferred because the fix means defining the composed grade (base
official + latest adopted overlay per problem) at the snapshot seam — a change to the snapshot
contract and its diff baseline that needs its own design pass. Future fix: compose snapshots as
official + latest overlay per (student, problem); until then, a publish-preview warning counting
adopted overlays in the assessment would at least make resend-all informed.

**W7 — Stale seeded mask rects after an id-region edit (append-only seeding)** — Finalize seeds
the student_id/name id-regions into `mask_regions` with page_scope 'all', append-only: it dedupes
by exact rect equality but never removes previously seeded rows, so editing an id-region after a
finalize leaves the OLD rect masking alongside the new one — the code documents this verbatim and
says "revisit if the adjust-and-refinalize workflow becomes real" (`seedMaskRegions`,
`internal/scan/finalize.go:88-96`). The failure direction is over-masking, not a PII leak: a stale
rect can now sit on answer content, so the AI grades images with part of the work blacked out —
silent score depression with nothing flagged. Deferred because seeded rows carry no provenance
(no column links a mask rect to the id-region that spawned it), so nothing can safely distinguish
"my stale seed" from an operator-drawn rect, and auto-removing rects risks unmasking identity —
the fail-safe direction is to keep them. Future fix: record provenance
(`seeded_from_id_region_id`) on seeded rows so re-finalize can replace exactly its own seeds;
short of a migration, warn when 'all'-scope mask rects no longer match any current id-region.

**W8 — Disabling a provider while runs are in flight terminal-fails the remaining leaves** —
`handleUpdateProvider` (`internal/httpapi/providers.go:129-208`) flips `enabled` with no check for
active runs, and each leaf resolves its provider at execution time, treating missing/disabled as
TERMINAL — "won't heal by retrying" (`internal/grading/runner.go:390-394`). An admin disabling a
provider mid-run (billing scare, key rotation) silently converts the rest of a 1,500-leaf run into
permanent failures; retry-failed-only keeps failing until re-enable, and the leaves were chosen
terminal precisely so a MISSING provider doesn't spin — but a disabled one WOULD heal on re-enable,
which the terminal classification throws away. The launch side is now guarded (Task B2:
`provider_disabled` danger warning + 409 on create), but disable-time is not. Deferred because the
disable-side fix is queue semantics — either a confirm counting in-flight runs on this provider, or
parking (retryable) leaves when the provider row exists but is disabled — beyond a warning's scope.
Future fix: both halves — count pending/running runs whose method's latest version names the
provider and confirm at disable time; classify disabled-but-present as retryable so re-enabling
resumes the run instead of stranding it.

**W9 — Roster re-import email changes silently lock students out of open regrade threads** —
Roster import upserts keyed by student_id and overwrites email unconditionally
(`internal/roster/import.go:23-25`), while inbound regrade verification rung 3 requires the sender
to equal the CURRENT roster email — mismatch → `rejected_sender_mismatch`
(`internal/httpapi/regrade.go:122,207`). A mid-term re-import that changes an address (registrar
sync, typo fix) means a student mid-thread — replying from the address the grade email actually
reached — is now silently rejected with no student-facing notice and no TA-facing signal beyond a
rejected row; the B-H10 re-bind only re-arms tokens across a re-publish, which may never happen.
Deferred because honoring the OLD address would be wrong exactly when the change happened because
that address was wrong or compromised — the correct move is a deliberate notify/re-issue path per
affected student, a feature rather than a warning. Future fix: at import time, diff incoming
emails against students with open regrade requests or live publish items and show a confirm with
counts (no PII in logs); pair with W3's fix so a per-student resend to the updated address
re-establishes the thread.
*Status (2026-07-10 roster lifecycle):* **partially resolved** — the first half of the future fix
now exists: the import response reports `email_changed` as a count, and the Students page shows it
with a note about open regrade threads, so a mid-term address change is no longer silent. Still
open: the per-student notify/re-issue path itself, and W3's live-address resend that would let a
re-send re-establish the thread at the new address.

## Deferred features (audit 2026-07-16)

**D1 — Problem-scoped corrective runs as final-source overlays** — Round-based grading pins ONE
exam-wide final source (`assessments.final_source_kind`/`final_run_id`, migration 0035), and
`RecomputeOfficials` derives every unpublished official by joining strictly on
`grading_records.run_id = final_run_id` (`internal/store/queries/grading.sql`). That join has no
way to compose "assessment run A supplies problems 1-4, corrective problem-scoped run B supplies
problem 5" — pinning a problem- or answer-scoped run as the final source would silently
un-officialize every answer outside that run's scope, which is exactly the trap audit finding A4
found (`docs/audits/2026-07-16-publish-safety-review-and-usability.md`). The minimal fix landed in
the same pass (`internal/store/grading.go`'s `SetAssessmentFinalSource`): reject pinning any run
whose `scope_kind != 'assessment'` outright (`ErrFinalRunNotAssessmentScope`, surfaced as 422
`final_run_scope_not_assessment`). That closes the silent-un-officialize hole but reintroduces a
real workflow gap it used to (accidentally) support: the cheap "re-grade just the one broken
problem after a rubric fix" flow has no supported path — a TA must re-run the WHOLE assessment
against the fixed rubric to get a new pinnable assessment-scoped run, even when only one problem's
grading was wrong. Deferred because a real fix means extending the final-source model itself: a
composed source (one base assessment-scoped run + zero-or-more problem-scoped "overlay" runs, most
recent overlay wins per problem) needs its own schema (an ordered join table, not two nullable
columns), a new `RecomputeOfficials` join shape (per-problem source resolution instead of one
constant `final_run_id`), and picker/UI work to let a TA compose and reorder overlays sanely —
a design pass of its own, not a guard-rail. Future fix: add a `final_source_overlays` table
(assessment_id, problem_id, run_id, applied_at) recording problem-scoped runs layered on top of the
pinned assessment run; `RecomputeOfficials` resolves each answer's source record from its
problem's overlay row if one exists, else the assessment-wide `final_run_id`, with the same
current-rubric/unflagged/spot-check rules as today. Until then, the workaround is a fresh
assessment-scoped run over the whole exam.
