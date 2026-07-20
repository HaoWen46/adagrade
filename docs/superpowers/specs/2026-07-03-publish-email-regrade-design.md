# Publish + Email + Regrade (Phases 6–7) — design

*2026-07-03 overnight session. Closes plan §7 / Phases 6–7. Decisions here follow the
DECISIONS.md convention: chosen v0 defaults are flagged `(D28…)` and harvested into
that file; the user reviews them after the fact, as with the Phase 0–4 build.*

## 1. Goal

Grades computed inside ADA-Marker must reach students: a teacher **publishes** an
assessment, each roster student receives an **individually-addressed email** with their
total, per-criterion breakdown, and comments, carrying a **signed regrade token** in the
Reply-To address. Student replies arrive via the email provider's **inbound webhook**,
are verified against the token and roster, rate-limited, and land in a **regrade queue**
that TAs resolve. This is the terminal step the plan calls the product's whole point.

## 2. Publish model (D28)

Publishing is **per assessment**, snapshot-based, and append-only — matching the
grading-records philosophy.

```
publish_batches   one row per publish action (assessment, actor, note, created_at,
                  superseded_at)
publish_items     one row per included student: snapshot JSONB (per-problem totals,
                  per-criterion scores + comments, assessment total), recipient email
                  (roster email at publish time), regrade token, email_status
                  (pending|sent|failed|skipped), provider_message_id, sent_at, error
```

- **Coverage gate:** an assessment is publishable only when every (roster student ×
  problem) answer either has an official grade or is `no_submission`. The publish
  preview enumerates blockers (answers missing official grades) instead of failing
  opaquely.
- **Effects of publishing:** create batch + items, set `answers.published_at` for every
  answer of the assessment (the coverage gate already spans all of them; the existing
  D1/D6 guard column), enqueue one send job per item. Students whose every answer is `no_submission` get an item with
  `email_status=skipped` (visible in the preview) — no email.
- **Lock:** official-grade changes on a published answer are rejected (HTTP 409) while
  `published_at` is set. This turns the existing read-only guard into an enforced lock.
- **Unpublish (D29):** admin-only escape hatch — clears `published_at` on the batch's
  answers and stamps the batch `superseded_at`. Audit-logged. It does not un-send email;
  it re-opens grading so a correction + re-publish can happen.
- **Re-publish (D30):** creates a new batch. Default selection is **changed-only** —
  items whose snapshot differs from the same student's item in the latest prior batch —
  with an explicit "resend to everyone" toggle. The email template for a re-publish says
  "corrected results".

## 3. Email seam implementations (D31)

`internal/email` implements `domain.EmailProvider` three ways, selected by
`ADAMARKER_EMAIL_PROVIDER`:

| value      | behavior | use |
|------------|----------|-----|
| `file` (default in development) | writes RFC-5322 `.eml` files under `<blobdir>/../outbox/` and logs the path | dev, tests, tonight's demo |
| `smtp`     | STARTTLS (587) or implicit TLS (465) via stdlib `net/smtp` + `crypto/tls`; `ADAMARKER_SMTP_HOST/PORT/USER/PASS` | any university or personal mailbox |
| `postmark` | Postmark HTTP API via stdlib `net/http`; `ADAMARKER_POSTMARK_TOKEN`; `ParseInbound` decodes Postmark's inbound JSON | the plan's intended production provider |
| `none`     | publish records everything but marks items `skipped` with a loud startup + publish-time warning | grading without notification |

Shared config: `ADAMARKER_EMAIL_FROM` (required for smtp/postmark),
`ADAMARKER_EMAIL_REPLY_DOMAIN` (the inbound domain for `regrade+<token>@…`; when unset,
Reply-To is omitted and emails say replies are not monitored — smtp mode without an
inbound pipe is still useful). Production + provider≠none requires FROM set, else the
config loader fails loudly, mirroring the OAuth rules.

**Content:** text + minimal HTML alternative, built with `text/template` +
`html/template`: assessment name, per-problem lines (criterion → score/max, comment),
assessment total, regrade instructions with expiry date. Subject:
`«assessment name» — results`. **PII rule:** message bodies are never logged; logs carry
only counts, statuses, and item ids (CLAUDE.md privacy rule).

**Send pipeline:** one River job per publish_item on a dedicated `email` queue,
rate-limited (default 1/s, `ADAMARKER_EMAIL_RATE` overridable), retries with River's
backoff, terminal failure ⇒ `email_status=failed` + error text. A "resend failed"
action re-enqueues failed items only. Shutdown-drain semantics follow F17 (a
drain-cancelled send stays `pending`, never `failed`).

## 4. Regrade token (D32)

`v1.<publish_item_id>.<expiry-unix>.<base64url(HMAC-SHA256(key, "v1|item|expiry"))>`

- Key: a subkey derived from the existing machine-local master key (D16) with
  HKDF(info="regrade-token-v1") — no new secret to manage.
- Expiry: `ADAMARKER_REGRADE_WINDOW` (default 14 days from send).
- The token identifies a publish_item (⇒ student + assessment + snapshot). It is not
  single-use: repeats are governed by the rate cap below. Stored on the item for
  display/debug; verification is by recomputation, not lookup, so the webhook path
  cannot be used to enumerate items.

## 5. Inbound webhook + verification ladder (D33)

`POST /webhooks/email/inbound/{secret}` — the path secret comes from
`ADAMARKER_INBOUND_WEBHOOK_SECRET`, compared constant-time; 404 on mismatch (no oracle).
Body size-limited per F5 conventions. The handler calls `EmailProvider.ParseInbound`,
then walks the ladder; every rejection is recorded on the request row with a reason,
**and no reply email is sent for unverified mail** (no backscatter):

1. token parses + HMAC valid + not expired,
2. its batch is not superseded,
3. sender email equals the **current roster email** for the token's student
   (case-insensitive; roster-changed-email therefore invalidates old addresses —
   B-H10's roster-change answer),
4. SPF/DKIM verdicts recorded; v0 **warn-not-block** (flagged for review — university
   forwarders break strict enforcement),
5. rate cap: at most `ADAMARKER_REGRADE_MAX` (default 3) *verified* requests per
   (student, assessment); beyond ⇒ status `rejected/rate_limited`, still visible in the
   queue per the plan's escalation requirement.

Verified requests get status `received`, a confirmation email (same pipeline), and a row
in the queue.

## 6. Regrade queue

`regrade_requests`: publish_item/student/assessment refs, received_at, from_email,
spf/dkim verdicts, subject, body text (student content — same PII class as
transcriptions, never logged), status
`received → under_review → resolved_upheld | resolved_regraded` (plus `rejected/*` from
the ladder), resolver, resolution_note, resolved_at.

**UI — "Regrades" page:** queue with status filters and per-assessment grouping; detail
view shows the email, the student's published snapshot, and links to the AnswerView for
re-grading; resolve actions = uphold / regraded (with note). Resolving sends a
resolution email; if the grade changed, the assessment shows as needing re-publish
(changed-only re-publish picks the student up automatically). Whether a regrade may
lower a grade (B-H15) is a policy choice the UI surfaces as a warning, not a block (D34).

## 7. API surface

```
GET  /api/assessments/{id}/publish/preview   coverage, blockers, changed-vs-last, skips
POST /api/assessments/{id}/publish           {note, resend_all} → batch (admin/teacher)
POST /api/assessments/{id}/unpublish         admin-only, audited
GET  /api/assessments/{id}/publish/batches   history + per-item statuses
POST /api/publish/batches/{id}/resend-failed
POST /webhooks/email/inbound/{secret}        provider webhook (no session auth)
GET  /api/regrades?status=&assessment=       queue
GET  /api/regrades/{id}                      detail (email body, snapshot, links)
POST /api/regrades/{id}/resolve              {outcome, note}
```

All state changes audit-logged (`publish.create`, `publish.unpublish`,
`regrade.resolve`, …).

## 8. Schema (migrations 0016, 0017)

0016_publish.sql: `publish_batches`, `publish_items` (+ indexes on batch, student,
status), trigger-free — locking uses `answers.published_at` as today.
0017_regrade.sql: `regrade_requests` (+ index on (assessment_id, status), on
(student_id, assessment_id) for the rate cap).

## 9. Testing

- Token: round-trip, tamper, expiry, wrong-key — table-driven unit tests.
- Providers: `file` asserted on disk; `smtp` against an in-process SMTP test server;
  `postmark` against `httptest` with recorded request assertions; `ParseInbound` with
  fixture JSON (fake names only — no real student data in fixtures, per CLAUDE.md).
- Publish: integration tests for coverage gate, snapshot correctness, lock enforcement,
  changed-only re-publish, resend-failed idempotency.
- Webhook: every rung of the ladder, the rate cap, and the no-backscatter property.

## 10. Out of scope (recorded in PLAN_GAPS)

Bounce/complaint webhooks, digest emails, a student-facing portal, DKIM signing for
smtp mode (delegated to the relay), regrade auto-grading.
