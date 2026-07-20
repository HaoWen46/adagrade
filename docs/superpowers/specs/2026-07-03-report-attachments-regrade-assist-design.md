# Result-PDF attachments + regrade AI assist — design

*2026-07-03 morning round. User-requested refinements to last night's Phases 6–7
(publish/email/regrade). User approved building from this message; v0 defaults
flagged (D41…) per house convention.*

## 1. Goal

Grade emails should carry a **per-student result PDF** — each page shows the
student's handwritten page (left) beside its grading result (right), problems
merged in order, "kinda like some evaluation stuff." Operators choose between
compressed and original quality (or no attachment), with a ZIP-of-images
fallback. TAs can **resend to an individual student**, and the regrade queue
gains an **AI re-grade assist**: a stricter, context-loaded second opinion the
TA triggers per-request or for all eligible requests at once — students can
never trigger API spend themselves.

## 2. Sender identity (already built — clarification only)

`ADAMARKER_EMAIL_FROM` is the sender (e.g. `ada2026@csie.ntu.edu.tw`); SMTP
auth for that mailbox goes in `ADAMARKER_SMTP_*`. New here: the publish dialog
displays the From address so the operator sees what students will see (D41).

## 3. Report PDF (D42)

New package `internal/report` behind a small seam (`ReportRenderer`), impl
`github.com/go-pdf/fpdf` (UTF-8 TTF support) — a new sanctioned seam,
justified like Renderer/BlobStore (stdlib has no PDF writer; CJK comments
require real font embedding).

- **Layout:** A4 landscape per answer page: left half = the student's page
  image (**original, unmasked** — masking exists only for LLM calls; students
  see their own work); right half = grading panel: problem label, per-criterion
  `name score/max` + comment, problem total; first page of the PDF carries a
  header (assessment name, student name+ID, assessment total). Multi-page
  answers: image pages run sequentially; the panel renders on the problem's
  first page, later pages show "(continued)".
- **Fonts:** Noto Sans TC via `make report-fonts` into `data/fonts/`
  (downloaded like OCR models, never committed), env
  `ADAMARKER_REPORT_FONT=./data/fonts/NotoSansTC-Regular.ttf`. Feature-gated
  like local OCR: unset ⇒ attachment options disabled in the UI with a hint;
  publish without attachments still works (D43).
- **Quality options — exactly three (D44):**
  | option | meaning |
  |---|---|
  | `none` (default) | today's text-only email |
  | `compressed` (recommended) | page images downscaled to long edge 1600px, JPEG q75 |
  | `original` | page images as stored (render DPI/limits from ingest) |
- **ZIP fallback (D45):** a checkbox swaps the merged PDF for a ZIP of
  per-problem JPEGs at the chosen quality — for mail-gateway or PDF-viewer
  trouble. Filenames inside: `problem-<n>-page-<m>.jpg` + `grades.txt`
  (the text body's breakdown), no PII beyond the student's own content.
- **Size guard:** attachments are built at publish time per item; any item
  whose attachment exceeds 15 MB gets a per-item warning in the batch view
  (send still proceeds; SMTP servers reject with a visible failed status if
  over their limit).
- **Plumbing:** `domain.OutboundEmail` gains `Attachments []Attachment`
  (`{Filename, MIME string, Content []byte}`); smtp/file providers emit
  `multipart/mixed` (alternative part nested); postmark maps to its
  attachments array; `none` unchanged. CRLF guard extends to filenames.
  Attachment bytes are built in the send job (not stored in publish_items —
  blobs stay the source of truth; rebuild-on-resend is deterministic).
  `publish_batches` records the chosen `attachment` + `zip` flags so resends
  reuse them.

## 4. Individual resend (D46)

`POST /api/publish/items/{id}/resend` (lecturer+, audited
`publish.resend_item`): re-enqueues that item's send job with the batch's
attachment settings regardless of current status (covers "student says they
never got it"). UI: a per-row "Resend" button in the batch history table.

> Note: §1's "TAs can resend to an individual student" is loose phrasing
> carried from the goal statement — this §4 access rule (lecturer+, matching
> the rest of the publish route family) is normative and is what's
> implemented; TAs cannot call this endpoint.

## 5. Email copy (D47)

The template's regrade instructions become explicit: "To request a regrade,
**reply directly to this email** (keep the subject line), describe which
problem and why, before <deadline>." Only replies carry the token; the copy
now says exactly that.

## 6. Regrade turn semantics — escalate, don't reject (D48)

Rung 5 changes: verified requests at turn ≤ `ADAMARKER_REGRADE_MAX` (3) are
`received` as today; turns MAX+1..HARD_MAX become **`received` +
`escalated=true`** — visible, AI-assist disabled, badge "manual review
required" (the plan §7's original intent). Beyond
`ADAMARKER_REGRADE_HARD_MAX` (default 10) ⇒ `rejected/rate_limited` as today
(table-flood backstop). Migration adds `escalated BOOL` + `turn INT` to
`regrade_requests`; turn = 1 + prior verified count for (student, assessment).

## 7. Reply→problem matching (D49)

Best-effort parse of subject+body for problem references — `problem 3`,
`q3`/`p3`, `#3`, `第3題/第三題`, `问题3` (CJK numerals 1–20 mapped) — against
the assessment's problem numbers. Stored as nullable
`regrade_requests.problem_id` guess; the detail UI shows it as an editable
chip (`PATCH /api/regrades/{id}` `{problem_id}`), and the deep-link goes to
that problem's AnswerView when set. Wrong guesses cost one click to fix;
no guess ⇒ the existing answer-list links.

## 8. AI re-grade assist (D50)

- **Prompt:** a dedicated versioned template kind `regrade_v1` (seeded like
  grading templates, read-only firmware per D25 philosophy), single stance =
  **stricter**: context = rubric (pinned version), reference solution, the
  original per-criterion scores + comments (from the contested official
  record), and the student's request text; framing: "an independent stricter
  re-examination — change a score only on demonstrable grading error in
  either direction; skepticism toward unsupported claims; do not reward
  persistence." Output schema = the standard grading JSON.
- **Privacy (D51):** identity XOR content law holds. Images sent are the
  existing **masked** copies (sealed `ProviderImage` path). The student's
  request text is redacted before prompt assembly: exact-match removal of
  that student's roster name, student ID, and email (plus the `regrade+…`
  token string); redaction is mechanical and logged as counts only.
- **Execution:** River jobs on the existing `llm` queue (provider rate
  limits apply). Result = append-only grading record `source='regrade_ai'`,
  policy pinned `regrade_strict`, linked via `regrade_requests.ai_record_id`.
  **Never** auto-official — the TA compares old vs new in the regrade detail
  and walks the normal (unpublish→official→re-publish) path; a "needs
  re-publish" chip already exists.
- **Buttons (D52):** per-request "AI re-grade" (eligible: status
  `received`/`under_review`, `escalated=false`, no AI record yet or explicit
  "re-run"); queue-level "AI re-grade all pending" enqueues for every
  eligible request (skips ones with an AI record). Both TA-clickable only —
  students cannot cause spend. Batch button shows the count + estimated cost
  (per-model pricing × answers heuristic) before confirming; run-level cost
  caps do not apply (these are single-leaf jobs) but monthly budget checks do.
- **Method/model:** uses the method version pinned on the contested record
  (same models the original grade used), keeping comparisons apples-to-apples
  (D53). If that method's provider is gone, the request shows "AI unavailable
  — provider removed" and stays manual.

## 9. API surface

```
POST  /api/publish/items/{id}/resend            (D46)
PATCH /api/regrades/{id}                        {problem_id} (D49)
POST  /api/regrades/{id}/ai-regrade             enqueue one (D52)
POST  /api/regrades/ai-regrade-all              {assessment_id} enqueue eligible (D52)
```
Publish request gains `{attachment: "none"|"compressed"|"original", zip: bool}`.

## 10. Schema (migrations 0022, 0023)

0022_publish_attachments.sql: `publish_batches.attachment TEXT NOT NULL
DEFAULT 'none'`, `publish_batches.zip BOOL NOT NULL DEFAULT false`.
0023_regrade_assist.sql: `regrade_requests.turn INT`, `.escalated BOOL NOT
NULL DEFAULT false`, `.problem_id BIGINT REFERENCES problems`,
`.ai_record_id BIGINT REFERENCES grading_records`; backfill `turn` from
verified-count ordering.

## 11. Testing

Report: layout smoke via pdfium round-trip (render the generated PDF back to
images — we own a renderer; assert page count + non-blank halves), image
downscale math, CJK comment renders (font-gated test), 15MB warning math,
multipart/mixed + postmark attachment mapping, ZIP fallback contents.
Regrade: turn/escalation ladder tests updated (turn 4 received+escalated, 11
rejected), problem-parse table (EN/ZH numerals), redaction (name/ID/email/token
excised; counts logged), ai-regrade eligibility matrix, ai-regrade-all skips
escalated + already-graded, record linkage, budget-409 on batch button.

## 12. Out of scope (recorded)

Regrade auto-resolution, attachment re-download portal, bounce handling
(still open), per-problem regrade tokens (token stays assessment-scoped;
problem matching is heuristic + human-corrected).
