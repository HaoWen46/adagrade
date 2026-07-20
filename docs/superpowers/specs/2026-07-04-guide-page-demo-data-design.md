# Guide page + demo data — design spec

*2026-07-04. Small feature: an in-app usage guide and synthetic demo data for the
full grading workflow (approved design; language English; delivery = generator
script + committed outputs).*

## 1. Guide page

- New sidebar entry **Guide** below Regrades; route `/guide`; component
  `frontend/src/pages/GuidePage.tsx`. Static content only — no backend changes, no
  new dependencies; existing `ui.tsx` primitives (`Card`) and Tailwind.
- Sections, each an anchored `Card` (`id=` on the card wrapper so `/guide#identify`
  deep-links):
  1. **Overview** — what ADA-Marker is; a pipeline strip rendered as styled
     flex divs: Roster → Assessment & rubrics → Collect work → Mask → AI grade →
     Review → Publish → Regrades.
  2. **One-time setup** — import the roster (Students page, CSV `student_id,name,email`);
     add Providers (API keys, models, pricing — needed for AI grading and scan OCR);
     Methods & grading policies (lenient/standard/strict stance).
  3. **Per-assessment workflow** — ordered walkthrough of the tabs:
     Problems & rubrics (Σ criteria = max points) → collecting student work via the
     two intake paths: **Submissions** (pre-sorted `<student_id>.pdf`, one per
     student) and **Identify** (scanner piles: draw the three header regions —
     student ID / name / problem — upload giant PDFs/zips, auto-assign only on
     ID+name agreement + valid problem number, orphan queue for the rest, parked
     duplicates/conflicts never overwrite, assessment-wide incremental Finalize) →
     Masking (review gate blocks runs) → AI runs & Review (accept/override,
     official pointer) → Consensus (post-hoc aggregation) → Totals → Publish
     (coverage gate, result emails with regrade tokens) → Regrades (reply-driven
     turns, TA verdicts).
  4. **Component reference** — one short definition-list entry per page/tab
     (Assessments, Students, Methods, Providers, Runs, Regrades, and each
     assessment tab) saying what it is for and when to touch it.
  5. **Try it with demo data** — step-by-step walkthrough wired to §2's files.
- Tone matches the existing HelpTip copy; PII rules trivially satisfied (static text).

## 2. Demo data

- Generator `scripts/make-demo-data.py`, executed as
  `uv run --with reportlab python scripts/make-demo-data.py` via a new Makefile
  target `demo-data`. Fixed RNG seed ⇒ deterministic bytes ⇒ the three outputs are
  committed under `data/demo/` and only change when the script does.
- **`demo-roster.csv`** — header `student_id,name,email`; 10 synthetic students
  `B11902001`–`B11902010`, invented Chinese names, `@demo.example` emails. Never
  real people.
- **`demo-exam.pdf`** — "ADA Demo Exam": page 1 is the answer-sheet template with
  the three EMPTY header boxes at the top strip (normalized positions: student ID
  x 5–30%, name x 35–60%, problem x 65–90%, y 2–8%) plus box-filling instructions —
  directly usable as the region-editor template image; pages 2–5 are four short
  algorithm problem statements (binary-search complexity, greedy interval
  scheduling, LIS dynamic programming, BFS shortest path), each with max points,
  written so rubrics are easy to draft.
- **`demo-scan-pile.pdf`** — 40 pages = 10 students × problems Q1–Q4, page order
  SHUFFLED (deterministic) like a real feeder pile. Every page: the three header
  boxes filled (printed text standing in for handwriting: ID, Chinese name,
  `Q1`…`Q4`) + an answer body of deliberately varied quality, deterministically
  assigned so the mix is stable: roughly 50% plausible multi-line answers, ~15%
  confidently wrong answers, ~15% too-short one-liners, ~10% blank body
  (header only), ~10% off-topic/garbled — so downstream AI grading, flags, and
  review flows have something real to disagree about.
- The Guide's §5 walkthrough: `make demo-data` (or use committed files) → import
  `demo-roster.csv` → create an exam with problems 1–4 (10 pts each) → draw the
  three regions using `demo-exam.pdf` page 1 as the local template image → upload
  `demo-scan-pile.pdf` in Identify (OCR on with a configured provider, or off for
  the manual path) → resolve orphans → Finalize → Masking → AI runs.

## 3. Verification

Frontend typecheck + build; real browser render of `/guide` (post-crash lesson:
static gates don't catch render errors); live dev-server run feeding
`demo-scan-pile.pdf` through the Identify tab against the imported demo roster.

## 4. Out of scope

Bilingual content; in-app markdown rendering; auto-seeding the demo data into the
DB; screenshots/video in the guide.

## 5. Completed-state seeder (demo-polish plan 2026-07-10, Task SEED)

`scripts/seed-demo-walkthrough.py` (`make demo-walkthrough`) builds the missing
happy-path endpoint of the walkthrough: an assessment named **"Demo Exam —
completed"** seeded into the RUNNING dev server (:8899, dev login) purely
through the public HTTP API — so the script doubles as an end-to-end smoke
test. It imports `make-demo-data.py` as a module and replays the SEED=46 rng
sequence, so the per-student PDFs it uploads carry exactly the committed scan
pile's answer bodies, just via the deterministic **Submissions** path (page i →
problem i positional mapping) instead of Identify/OCR. Steps: problems +
tier-matched rubrics + reference solutions → id-regions and the derived
student_id/name mask regions (mirroring finalize's D66 seeding) → per-student
uploads polled to mapped-complete → masks applied and bulk-accepted →
`materialize-answers` so non-demo roster students publish as skipped → an AI
run on the cheapest configured "flash" method (entering docs/MODELS.md pricing
first when absent, so run cost is real) → spot-check verdicts → final source →
publish (file provider outbox) → two regrade threads POSTed to the real
inbound webhook as Postmark-shaped payloads, tokens extracted from the outbox
`.eml` Reply-To headers. Idempotence is skip-not-merge; `--no-ai` stops before
the run, `--continue` resumes steps 5-7 on the existing assessment (each step
skips itself when its outcome already exists), `--regrades-only` re-drives only
step 7.

Regrade caveats: `scripts/dev-e2e.sh` now defaults
`ADAMARKER_INBOUND_WEBHOOK_SECRET` (`dev-webhook-secret`) and
`ADAMARKER_EMAIL_REPLY_DOMAIN` (`regrades.dev.local`) so the webhook route
exists and grade emails carry tokens — a server started before those defaults
needs a restart, then `--regrades-only`. Separately, the dev `file` email
provider's `ParseInbound` always errors (internal/email/file.go), so the
webhook 400s any payload under that provider; until the file provider learns to
parse the Postmark inbound shape, the seeder reports this and leaves the
regrade inbox empty rather than faking rows through the DB.

## 6. Edge-path demo artifacts (2026-07-11)

`make-demo-data.py` also emits a second family of deterministic artifacts for
demoing the UNHAPPY paths. They use a separate `random.Random(SEED2=47)` so the
original three SEED=46 outputs stay byte-identical; all names/emails remain
synthetic. Each artifact maps to one UI flow:

- **`demo-roster-v2.csv`** — the week-3 add/drop re-import (Students → Import).
  Against a `demo-roster.csv` baseline the import diff reports
  `missing_active = [B11902009, B11902010]` (dropped — the "Withdraw all 2"
  bulk-sync button), two added students (B11902011 宋竹青, B11902012 林一帆),
  `email_changed = 1` (B11902003 moved to a `@student.demo.example` address) and
  `name_changed = 1` (B11902007 真 → 眞 spelling fix). Re-importing it after
  those two adds were withdrawn instead demos the `withdrawn_present`
  retaker-trap warning.
- **`demo-roster-mistakes.csv`** — a roster the importer REJECTS whole (D13)
  with a readable per-line error list, one mistake per concept (8 rows): line 5
  collides with line 4 under `studentid.Normalize` (`b11902003` vs the
  full-width `Ｂ１１９０２００３`), line 7 shares line 6's email, line 8 has an
  invalid email (no `@`), line 9 an empty name.
- **`demo-roster-big5.csv`** — the same rows as `demo-roster.csv` saved the way
  Chinese-locale Excel does by default (Big5/CP950); demos the whole-file
  "not valid UTF-8 — use Save As → 'CSV UTF-8 (Comma delimited)'" rejection.
- **`demo-scan-pile-messy.pdf`** — a 12-page pile for the Identify tab's edge
  paths: 6 normal pages, 2 duplicate pages (same student+problem as two normal
  pages but different answer text → the parked/conflict path), 1 empty and
  1 pen-scribbled student-ID box plus 1 unknown id `B99999999` (→ orphans), and
  1 fully blank page. Page tiers are fixed so the duplicates provably differ;
  only the page order is rng-shuffled.
- **`demo-submissions/<student_id>.pdf`** — one 4-page PDF per roster student,
  pages in problem order (Submissions path positional mapping), carrying the
  same SEED=46 answer bodies as the committed scan pile — a manual drag-drop
  demo of the Submissions flow without running `seed-demo-walkthrough.py`.

Verified end-to-end against the running dev server: mistakes → 400 with the
four expected per-line errors; big5 → 400 with the UTF-8 message; v2 → 200 with
the expected diff (then restored by re-importing `demo-roster.csv`; the two
added students have no delete endpoint, so they are left withdrawn).
