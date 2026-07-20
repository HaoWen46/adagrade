# ADA-Marker — AI-Assisted Grading System

*ADA-Marker* is the successor to the course's old *Ada Judge* autograder. Where **Judge**
rendered pass/fail verdicts on code, **ADA-Marker** uses AI to **mark** handwritten exams and
assignments against rubrics — with partial credit, comments, and human review. "ADA" is the
course (Algorithm Design & Analysis); "-Marker" names the grading role, echoing "Ada Judge".
The AI is the mechanism, deliberately kept out of the name so the name outlives the models.

> This document is the source of truth: **why** the system exists and **what** it must do.
> It is deliberately light on low-level implementation so the coding agent can research and
> choose concrete approaches. Where the agent finds a clearly better implementation, prefer
> it — but keep the **objectives** and the **shape of the domain** intact.

---

## 1. Scope — what this is (and isn't)

ADA-Marker is **one system that does one job: grading.** There is no "course" concept and no
multi-course / multi-tenant abstraction — do not add one.

The top-level unit is an **Assessment**, which is either an **exam** or an **assignment**.
Expect roughly **3 exams and 3 assignments**. The team can **add or remove assessments from
within the app**, but because removing one destroys submissions, grades, and grading history,
this is a guarded, deliberate action (see §10), not a casual button.

Students do **not** have accounts. They receive grades and file regrades entirely by
**email**. The only people who log in are the authorized team: lecturer(s) and TAs.

Three things that were under-specified before and are now central:

- **Grading approaches are data, not code.** TAs/devs must be able to define and try new
  grading methods in the app — different models, prompts, reasoning on/off, rubric variants —
  **without editing code**.
- **Grading is run-based and keeps history.** Each grading pass on an answer is saved as a
  record. You can grade the same answer again with a different method and **compare all
  attempts in its history**. Nothing is one-shot or destructive.
- **Human review is full browsing, not a flag-only queue.** A grader can click into **any**
  submission, flagged or not; flagged ones are highlighted.

---

## 2. Objectives

1. **Ingest handwritten submissions.** Accept PDFs (usually one problem per page), render each
   page to an image, and associate each answer with the right student and problem.
2. **Grade with configurable methods.** Grade each answer against a structured rubric using a
   **grading method that is defined and editable in the app** (models, prompt, reasoning
   on/off, reference solutions included, multi-model agreement rule, etc.). No code change to
   try a new approach.
3. **Keep grading history + allow re-grading.** Every grading run on an answer is saved.
   Re-run with a different method; view and compare all runs in the answer's history;
   choose/override the official grade.
4. **Full human grading & review.** Browse any assessment → problem → student → answer. View
   the rendered submission alongside AI grade(s), AI comment(s), grading history, and regrade
   history. Grade or override manually; manual grades are just another record in the history.
5. **Distribute grades by email.** Send each student their score, per-criterion breakdown, and
   comments.
6. **Handle regrades by email.** Students reply to request a regrade; route it back to the
   right answer, re-grade and/or queue for a human, escalate to mandatory human review after a
   threshold.
7. **Authorized access only.** Whitelisted lecturers and TAs, role-based.
8. **Measure & improve.** Records carry the method, prompt version, model, confidence, and
   cost, so approaches can be compared across runs and exams.

Non-goals for v1: student logins, multi-course support, mobile app, plagiarism detection, LMS
integration.

---

## 3. Domain Model

Relationships matter more than exact schema.

- **Assessment** — `type` = `exam` | `assignment`; name; ordered list of Problems. Add/remove
  from the app under guardrails (§10).
- **Problem** — belongs to an Assessment; number, max points, one or more **reference
  solutions**, a **Rubric**, and a derived **status** (aggregate of its answers' statuses).
- **Rubric → Criteria** — a structured list; each Criterion has a description and points (plus
  partial-credit notes). Grading is **per criterion**, not holistic. Rubrics are versioned so
  records can reference the exact rubric used.
- **Student** — roster entry: name, student ID, email.
- **Submission** — a student's uploaded file(s) for an assessment (the source PDF).
- **Answer** — the atomic gradable unit: one `(student, problem)`. Holds the source PDF page
  and a rendered image (JPG) of it; usually a single page.
- **GradingMethod** *(a.k.a. grading configuration / approach)* — a **first-class,
  app-editable object** describing how to grade: which model(s), the prompt template,
  reasoning/CoT on or off, how many reference solutions to include, the multi-model agreement
  rule, the rubric version, etc. Reusable and versioned. **This is what makes new approaches
  data, not code.**
- **GradingRun** — one application of a GradingMethod to a **scope** (a single answer, a whole
  problem across students, or an entire assessment) at a point in time. Produces
  GradingRecords; recorded in history.
- **GradingRecord** — the result for **one answer from one run**: per-criterion scores, total,
  AI comment, the raw model output(s), confidence signals, token/cost, and the method + prompt
  + rubric versions used. **Many records accumulate per answer** across runs — this is the
  history.
- **Official Grade** — a selection pointing at the record (AI or human) that is the current,
  published grade for an answer. A human can set or override it.
- **HumanGrade** — a grade entered by a TA. It is simply a GradingRecord whose source is a
  human rather than a model, living in the same history.
- **RegradeRequest** — tied to an answer; tracks count, status, and escalation; its history is
  viewable in the answer view.
- **User** — TA / Lecturer / Admin; email; role; on the access allowlist.

Shape: Assessment 1—* Problem 1—* (Rubric/Criteria, reference solutions); Student 1—*
Submission 1—* Answer *—1 Problem; Answer 1—* GradingRecord (produced by GradingRuns using
GradingMethods); Answer 1—(current)→ one GradingRecord as the official Grade; Answer 1—*
RegradeRequest.

---

## 4. Grading: Methods, Runs & History (the core)

The earlier version wrongly baked in "one known pipeline." Correct model:

**Grading is parameterized by an editable GradingMethod and executed as runs whose records are
kept forever.** The app must let a TA/dev create, edit, and select methods and launch runs —
never require code edits to test a new approach.

**The prompt is a configurable, experimental surface — not a fixed recipe.** What is sent to
the model **probably** includes: the problem statement, one or more reference solutions, the
rubric/criteria, and grading instructions; it **may** include worked examples; and it **may or
may not** use a reasoning/CoT step or a reasoning model. All of these are **parameters of the
GradingMethod to experiment with**, and different methods will make different choices. Treat
"what goes into the prompt and whether the model reasons" as knobs, not constants.

**Running and re-running:**

- Pick a **scope** (one answer, a problem across all students, or a whole assessment) and a
  **GradingMethod** (existing, or newly defined in the app).
- Launch a **GradingRun** → it produces a GradingRecord per answer, **appended** to each
  answer's history.
- Want to try another approach? Define/pick a different method and run again. The new records
  sit **alongside** the old ones in history for comparison. **Nothing is overwritten.**
- A human can compare records, set the official grade to any of them, or enter a manual grade
  (also a record).

**Reliability principles** (principles the methods should support — not a single hardcoded
flow):

- **Per-criterion scoring** with a justification per criterion — explainable, comparable, good
  for feedback.
- **Fresh session per (answer, model)** so no student's answer leaks into another's grading.
- **Multi-model cross-check as one available strategy:** a method may run 2–3 models and define
  an **agreement rule** (e.g., agree per criterion, or within a tolerance on the total); on
  agreement auto-accept, on disagreement flag for a human. This is *a* configurable method, not
  *the* system.
- **Confidence / legibility signals** captured and used to flag answers for humans (illegible,
  blank, low model confidence) regardless of scores.
- **Structured, validated output** parsed against the rubric; malformed output is
  rejected/re-asked.
- **Everything recorded** (method, prompt/rubric versions, model, raw output, confidence, cost)
  so approaches are measurable and any grade is reproducible/explainable.

Ship a sensible **default method** so grading works out of the box — but the whole point is
that methods are editable objects you can multiply and compare.

---

## 5. The Web UI (browsable drill-down + human grading)

Human review is **full browsing**, not a flag-only queue. Any submission is clickable whether
flagged or not; flagged ones are highlighted. The primary navigation is a drill-down:

1. **Assessment picker** — choose an exam or assignment.
2. **Problem list** for that assessment — each problem shows its **status** (e.g., ungraded /
   AI-graded / has flags / human-finalized / published / open regrades) and progress.
3. **Student list** for the selected problem — rows of **name, email, current score, and
   status / flags / other info**. Flagged rows highlighted; all rows clickable.
4. **Answer view** for the selected student+problem — the heart of the grading screen. Shows:
   - the student's submission **rendered as an image (JPG)** for fast viewing, with the
     **source PDF** available (usually a single page);
   - the **AI grade(s)** and **AI comment(s)** from grading runs;
   - the **grading history** (all runs/records, comparable), with the current official grade
     indicated;
   - the **regrade history** for that answer;
   - controls for a human to **grade or override** (per criterion against the rubric), which
     creates a new record and can set the official grade.

Grading problem-by-problem across students (this navigation) keeps the standard consistent
within a problem. Also provide **method-management screens** (define/edit GradingMethods,
rubrics, reference solutions) and **run-launching controls** (pick scope + method → run).

---

## 6. Submission Ingestion

- Upload student PDFs; split into pages; **render each page to a JPG** at a
  legibility-appropriate DPI (DPI matters for handwriting — make it configurable); keep both
  the source PDF page and the rendered image on each Answer.
- Map each page to a `(student, problem)`. Assume the common case is **one PDF per student, one
  problem per page, in a known order**; support a per-assessment page-layout config for
  positional mapping and a **filename convention** (e.g. `<studentID>.pdf`) for student
  identity.
- Provide a **manual mapping-correction UI** as the safety net (blanks, skipped questions,
  answers spanning two pages, misordered pages). Real submissions are messy; treat that as
  normal.

---

## 7. Email (outbound + inbound)

- **Outbound:** individually addressed emails (no shared recipient lists) with total,
  per-criterion breakdown, and comments.
- **Inbound (regrades):** give every grade email a **unique reply address/token** mapping to
  `(student, answer)`; receive replies via the provider's inbound-parse webhook; **verify the
  sender matches the roster email**; create a RegradeRequest; apply policy (re-run a grading
  method and/or queue for a human); **rate-limit** to prevent abuse; **escalate to mandatory
  human review** after a threshold (default 3 on one answer) and disable further auto-regrades
  for it. Send an automated confirmation on receipt and on resolution.
- Use one provider that does both outbound send and inbound parse to keep it simple. The
  provider is an implementation detail; the objective is reliable, correlated, sender-verified
  two-way email.

---

## 8. Access Control

- **Auth:** Sign in with Google (university Workspace), restricted to an **allowlist** of
  authorized emails in the database.
- **Roles:** `Admin` / `Lecturer` — full access incl. managing assessments, methods, users,
  and publishing; `TA` — grade/override, work submissions and the review, define/try grading
  methods, handle regrades. Unlisted emails are denied entirely.
- Secure, http-only session cookies; standard web-security hygiene.

---

## 9. Tech Stack (recommendation + rationale)

**Backend: Go.** The workload is I/O orchestration — many concurrent, rate-limited LLM calls,
PDF handling, email send/receive, and DB work. Go's goroutines fit that, the ecosystem is
mature, and it's simpler and faster to build/maintain than Rust for glue-heavy services. Rust's
edge is CPU-bound work, which this isn't. **Verdict: Go.**

Building blocks (substitutable): lightweight HTTP router (`chi`); a **Postgres-backed durable
job queue** (e.g. `River`) so grading/ingestion/email jobs are async, retried, and need no
Redis; PDF via `pdfcpu` (split) + MuPDF (`go-fitz`) or Poppler `pdftoppm` (render to image);
Postgres driver + migrations.

**LLM provider layer (essential):** a provider-agnostic interface over vision-capable models,
selected by config, so GradingMethods can mix and compare models (e.g. Gemini, GPT-class
vision, Claude vision) **without code changes**.

**Frontend:** React + TypeScript + Tailwind + a component kit (e.g. shadcn/ui) for a clean UI
quickly; SvelteKit is a fine lighter alternative. Screens: the drill-down (§5), the
answer/grading view, method & rubric management, ingestion + mapping correction, and
publish/email controls.

**Database:** PostgreSQL. **Storage:** local disk volume for v1 behind a swappable interface
(source PDFs + rendered JPGs). **Email:** one transactional provider supporting outbound +
inbound webhook.

---

## 10. Add/Remove & Data-Safety Guardrails ("massive caution")

Because assessments own submissions, grades, and history, destructive actions must be hard to
do by accident:

- **Prefer archive / soft-delete** over hard delete; archived assessments disappear from normal
  views but retain data.
- **Block hard-deletion** of any assessment/problem that has submissions or grading records
  unless an Admin explicitly forces it.
- **Require explicit confirmation** for destructive actions (e.g., type the assessment name),
  and restrict them to `Admin` / `Lecturer`.
- **Never let a re-grade or regrade silently overwrite** a human-finalized/official grade
  without recording it in history.
- **Keep an audit log** of create / delete / override / publish actions and who did them.
- Adding assessments/problems is fine but deliberate; **changing a rubric after grading should
  create a new rubric version** rather than mutating existing records.

---

## 11. Build Phases

Ordered so the tool is usable early and the configurable/AI parts layer on top.

- **Phase 0 — Skeleton.** Repo, Go service, Postgres, migrations, Google OAuth + allowlist,
  roles, authenticated shell.
- **Phase 1 — Assessments & rubrics.** Create exams/assignments (with guardrails), problems,
  rubrics (criteria + points), reference solutions; import roster from CSV.
- **Phase 2 — Ingestion.** Upload PDFs, split + render JPGs, map pages → `(student, problem)`
  with the manual-correction UI, store PDF + image per answer.
- **Phase 3 — Browsable review + manual grading.** The full drill-down (§5) and the answer view
  with per-criterion manual grading/override. **Usable with zero AI** — key de-risking
  milestone.
- **Phase 4 — Grading methods & runs (single model).** GradingMethod as an editable object;
  launch a run over a scope; produce records into each answer's history; view/compare; set
  official grade. Prompt/reasoning as method knobs.
- **Phase 5 — Multi-model cross-check strategy.** A method that runs 2–3 models with a
  configurable agreement rule; auto-accept on agreement, flag disagreement/low-confidence.
- **Phase 6 — Publish + outbound email.** Finalize/publish; email students their breakdown;
  embed unique regrade tokens.
- **Phase 7 — Inbound regrade.** Webhook, token correlation, sender verification, RegradeRequest
  workflow, escalation, automated confirmations.
- **Phase 8 — Iteration tooling.** Reports over records: model/method agreement, human-override
  rate, cost per run, comparisons across methods and exams.

Each phase leaves the system working and demoable.

---

## 12. Open Questions

- Exact submission format and collection method (drives §6 mapping).
- Which models are permitted/affordable, and any data-handling constraints for student work
  (consider **redacting student identity** from what's sent to models — the grader needs the
  answer and rubric, not the name).
- Regrade policy: auto re-grade vs. straight-to-human, and the escalation threshold.
- Confirm the expected counts (~3 exams + 3 assignments) and whether assignments are used to
  calibrate grading methods before exams.
