# Per-student page — design (2026-07-28)

Read-only, staff-facing page about one student: their exam history, scores, and the
story around them (publish state, provenance, regrades). Students never log in; this
is the answer to "what's the story with this student" without a SQL session.

## Shape

Route **`/students/{student_id}`** — the school ID (`students.student_id`, TEXT
UNIQUE), the same vocabulary as the export filenames, the CSV, and the totals table.

```
header: name · ID · email · active/withdrawn
[card] exam — total · graded n/m          ← collapsed: name + problem rows w/ scores
       P1  title   10/10
       P2  title    8/10
       ── click card → expands in place (URL syncs ?assessment=) ──
       publish state · sent date · "changed since publish" badge
       per-problem provenance (human/AI, confidence, flags), published-vs-current
       regrade summary (request → per-problem verdicts)
[card] next exam …
```

- **Collapsed card** = name, official total, problem rows with scores. One cheap query.
- **Expanded detail** (lazy, per-exam endpoint) = everything that is not a score:
  publish/delivery state (`publish_items.email_status`, `sent_at`), the
  **changed-since-publish** badge (official grade now ≠ `publish_items.snapshot` at
  send time — the "what does the student believe vs. what is true" question),
  per-problem **provenance** (`human`/`model`+model id, confidence, flags — a bare
  overridden 8/10 reading as AI-given is a lie by omission), and regrade threads.
- **Click a problem row** → the existing `/answers/{id}` grading page. This page
  duplicates nothing: no images, no transcription, no grading actions — those all
  already live in AnswerView. Zero mutations here.

Entry points: Results→Totals student cells link here carrying `?assessment=`;
the Students roster page name cells link here too.

## API

- `GET /api/students/{sid}` — header + per-assessment summaries incl. problem rows.
- `GET /api/students/{sid}/assessments/{aid}` — expanded detail.

Decimal scores are strings (D4, `store.NumStr`); a student/problem with no graded
answer is `null`, never a fake 0 (D3). Absent answers render "absent" and are not
clickable. Logging is IDs-only (D14). Both endpoints are ordinary authed staff routes;
TA data-scoping (B-M15) stays open repo-wide — this page inherits AnswerView's
exposure, does not widen it, and makes it more visible (recorded, not changed, here).

## Deliberately out (v0)

Student-facing access of any kind; grading/publish actions; cross-student analytics
(ProblemReview's by-score view owns that); typeset math rendering of stored text.
