# Regrade v2 — multi-problem requests, single-use token chain, TA handoff

*2026-07-03 evening round. User-designed over an extended review of the v1 regrade
flow; supersedes the per-email-single-problem model of §5–§8 in
`2026-07-03-publish-email-regrade-design.md` and parts of
`2026-07-03-report-attachments-regrade-assist-design.md` §6–§8. Pre-production:
no live tokens exist, so v1 tokens/flows are REPLACED, not migrated. v0 defaults
flagged (D54…).*

## 1. Goal

One student reply = one turn = one request naming **all** problems they contest,
in a strict format. The request page stays per-email; adjudication and AI assist
run per-problem beneath it. Result emails are TA-clicked and hard-gated until
every listed problem has a verdict. Each system email in the chain is replyable
exactly once (single-use per-turn tokens). After the final turn, contested
problems hand off to their assigned TA person-to-person and the system goes
silent. Turn budget is configurable. The system NEVER auto-sends discretionary
student mail — only the filing confirmation is automatic.

## 2. Reply format (D54)

Exact, lowercase, ASCII: `<pN>` … `</pN>` per contested problem, N = the
problem number as printed on the exam. Everything between the tags is that
problem's complaint text, verbatim (multi-paragraph safe — an embedded `2:` is
just text).

```
<p1>
The base case n=1 was marked wrong, but rubric line 2 says:
2: partial credit applies when ...
</p1>
<p4>
My exchange argument handles ties — the -2 assumes it doesn't.
</p4>
```

Parser contract (pure pkg `internal/regrade`, table-tested):
- `>`-quoted lines are stripped BEFORE matching (our own template quoted in the
  reply must never self-match).
- Exact match only: `<q1>`, full-width `＜ｐ１＞`, Cyrillic lookalikes, uppercase,
  inner spaces, unclosed `<p1>` without `</p1>` ⇒ that block is **silently
  ignored**. No normalization, no heuristics, no fallback, no notice (D55 —
  the format is the contract; violations are on the student). The v1 prose
  heuristic (`match.go` problem guessing) is retired from the inbound path.
- Unknown N (no such `problems.number` in the token's assessment) ⇒ block
  silently ignored.
- Duplicate `<pN>` blocks concatenate in arrival order.
- Text outside all tags is ignored (greetings/signatures).
- ≥1 valid block ⇒ request FILES (token consumed); invalid blocks evaporate and
  are never mentioned in any later email.
- 0 valid blocks ⇒ nothing files, token NOT consumed (see §4), row recorded as
  `unparsed` for the queue's Unparsed filter (§7).

## 3. Translation layer (D56)

Deterministic, no subject parsing: token → publish_item → batch → assessment;
`pN` → `problems.number = N` within that assessment → `problem_id` (unique per
assessment). Renumbering a published assessment's problems re-points old
tokens' `pN` — recorded operator rule: don't renumber after publish (existing
known-gap).

## 4. Single-use per-turn token chain (D57)

Token v2: `v2.<publish_item_id>.<turn>.<expiry-unix>.<b64url HMAC-SHA256(key,
"v2|item|turn|expiry")>`; same HKDF subkey. v1 tokens are rejected
(pre-production, none live).

- The grade email carries token turn=1. **Result email #N** carries token
  turn=N+1. Confirmation and reminder emails carry **no token and no
  Reply-To** — replying to them physically cannot enter the pipeline.
- A token is **consumed** exactly when a request with ≥1 valid block files
  against it (enforced by a partial unique index on
  `regrade_requests(publish_item_id, turn)` for filed rows). Later replies
  carrying a consumed token are recorded as `addendum` rows (dimmed in the
  queue under the filed request), no processing, no confirmation, no turn
  burned. This replaces v1's count-based rate cap and kills the
  concurrent-double-reply TOCTOU structurally: two racing replies to one token
  → one files, one becomes an addendum via the unique index.
- All-garbage reply (0 valid blocks) does NOT consume the token — consuming
  without filing would end the chain with no result email and no next token,
  stranding the student with no recovery. Recorded as `unparsed`, total
  silence (D58).
- Turn max: `ADAMARKER_REGRADE_MAX` (existing env, default 3; lecturer may set
  2 or 5 — value read at receipt time, in-flight tokens carry their turn so a
  mid-term change stays coherent). Turns 1..MAX adjudicate. Result #MAX
  carries token turn=MAX+1 whose consumption fires the **handoff** (§6), not
  adjudication. `ADAMARKER_REGRADE_HARD_MAX` and the escalated band are
  retired — the chain structurally bounds volume (one reply per issued email;
  everything else is addenda/unparsed).

## 5. Per-problem adjudication + gated result email (D59)

- Schema: requests keep one row per email; new `regrade_request_problems`
  sub-items `(request_id, problem_id, complaint_text, ai_record_id, ai_error,
  verdict upheld|regraded|NULL, verdict_note, verdict_by, verdict_at)` —
  UNIQUE(request_id, problem_id). The v1 single `problem_id`/`ai_record_id`
  columns on requests are dropped (0025 migration). AI assist re-scopes to one
  sub-item per job — the LLM sees THAT problem's masked pages, rubric,
  reference, original grades, and THAT problem's complaint text only. The
  "no problem tag ⇒ fan out to all answers" behavior is deleted. Budget gate
  (shared helper) prices per sub-item; ai-regrade-all enumerates eligible
  sub-items. TA may still manually ADD a sub-item on a FILED request
  (`POST /api/regrades/{id}/problems`, escape hatch, audited) — never on
  unparsed rows. There is no edit/delete-sub-item endpoint; a "correction" is
  add-the-right-one (if missing) + uphold-the-wrong-one via the normal
  per-problem verdict, not an in-place edit.
- **Result email is TA-clicked** (`POST /api/regrades/{id}/send-result`) and
  hard-gated server-side: 409 until EVERY sub-item has a verdict. No bulk
  path may bypass (the gate lives in the send handler + a store-level check).
- Result email #N content: standalone numbered subject
  (`«assessment» — regrade result #N`), per-problem sections (quoted complaint
  → outcome word → TA note → new score when regraded), attempt counter
  ("attempt N of MAX"), next-turn token Reply-To + the format template, and
  for #MAX: "this was your final attempt; further replies go to the problem's
  TA directly." Sending resolves the request (`resolved`), audited
  (`regrade.send_result`). PDF stays with corrected-results re-publish (v1
  decision, unchanged).
- Confirmation email (automatic, on filing only): lists WHICH problems filed
  (numbers only), attempt counter, "you'll receive a result email; replies to
  this confirmation are not processed." No token.

## 6. TA-per-problem assignment + final-turn handoff (D60)

- `problem_ta_assignments (problem_id UNIQUE, user_id TA+, assigned_by,
  assigned_at)` — at most one TA per problem, one TA may own many. UI: picker
  on the assessment's problems editor; assignment state visible on regrade
  rows. Publish preview warns (not blocks) on unassigned problems.
- Consuming the handoff token (reply to result #MAX with ≥1 valid block):
  request records as `handed_off`; per contested problem, the assigned TA
  receives an email (via the normal provider): assessment/problem, student
  name+ID+email, that problem's complaint text, the student's full prior-turn
  history for that problem (verdicts+notes), and an app deep link. This is
  deliberate PII-to-authorized-grader over email (D61 — same trust class as
  grade mail; first internal mail carrying student data). One email per
  (TA, student, problem-group): a TA assigned two contested problems of the
  same student gets ONE email covering both. Multiple students ⇒ separate
  emails; the TA responds to each personally from their own mailbox.
- Problems with NO assigned TA in a handoff request: flagged
  "no TA assigned" on the row (lecturer-visible); no email (no target).
- After handoff the system is permanently silent for that thread: no further
  tokens are issued; anything else inbound records as addenda. Audit:
  `regrade.handoff` per notified TA.

## 7. Unparsed rows + TA-clicked reminder (D62)

Unparsed rows (0 valid blocks, live token) appear under an "Unparsed" queue
filter, dimmed, with student/assessment/which-email-of-the-chain and the raw
text. Button **Send reminder** (TA+, once per row — disabled after send,
audited `regrade.remind`):

- Anchored, never generic: names the assessment, the exact subject + sent date
  of the email whose token is still live, states the attempt was NOT used,
  includes the literal format template and the attempt counter, and says to
  reply **to that result email, not to this reminder**.
- Carries no token/Reply-To (structurally reply-proof); replies to it land in
  the plain mailbox.
- Never automatic — preserves the no-backscatter posture; discretion is the
  TA's.

## 8. API surface (delta)

```
POST /api/regrades/{id}/send-result      TA+, 409 until all sub-items verdicted
POST /api/regrades/{id}/resend-result    TA+, send-failure recovery (whole-branch review F1);
                                          409 unless resolved AND result_sent_at IS NULL
POST /api/regrades/{id}/remind           TA+, unparsed rows only, once
POST /api/regrades/{id}/problems         TA+, add sub-item (escape hatch; no edit/delete)
PATCH /api/regrades/{id}/problems/{pid}  TA+, verdict {outcome, note}
POST /api/regrades/{id}/ai-regrade       re-scoped: {problem_id} per sub-item
POST /api/regrades/ai-regrade-all        enumerates eligible SUB-ITEMS
PUT  /api/problems/{id}/ta               lecturer+, assign/unassign TA
```
`POST /api/regrades/{id}/resolve` (v1 single-outcome) is removed. List/detail
JSON: requests carry kind (filed|addendum|unparsed|handed_off), turn, and
sub-items with per-problem fields.

## 9. Schema (migration 0025)

`regrade_request_problems`, `problem_ta_assignments`, requests: add `kind`,
drop `problem_id`/`ai_record_id`/`escalated` (turn stays), partial unique
index `(publish_item_id, turn) WHERE kind='filed'`. Token storage on
publish_items unchanged (tokens are recomputed per turn at send time, not
stored — verify-by-recomputation as today; drop the stored single token column
if nothing else reads it).

## 10. Testing (normative)

Parser table (every §2 rule incl. quoted-template self-match, embedded "2:",
full-width rejection, unclosed, unknown-N, duplicates, outside-text); chain
(consume-on-file, addendum-on-consumed, unparsed-no-consume, race → unique
index one-winner, turn-max handoff, MAX change mid-flight); send gate
(409 until all verdicted, sends once, audited); reminder (once, anchored
content, no token); TA assignment (uniqueness, picker RBAC, unassigned
handoff flag); AI re-scope (per-sub-item context isolation — problem 1's
prompt must not contain problem 4's complaint; budget gate per sub-item);
emails (all four templates: content anchors, attempt counters, token presence
matrix — grade/result yes, confirmation/reminder/TA-notify no). No PII in
logs/fixtures throughout.

## 11. Out of scope (recorded)

Per-assessment turn budgets (env-global only), auto-forwarding post-handoff
mail to TAs, reminder re-sends, v1 token compatibility, per-problem regrade
deadlines.
